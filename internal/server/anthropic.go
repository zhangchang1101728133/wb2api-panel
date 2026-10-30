// anthropic.go Anthropic Messages 兼容的**对话**端点 POST /v1/messages。
//
// 与 messages.go 的分工：那边是 /v1/search/messages（只做搜索，不调模型），
// 这里是 Claude Code 等 Anthropic 客户端真正用的对话入口。
//
// 实现方式：把 Anthropic 请求翻译成 OpenAI 请求，复用既有的 chatCompletions
// 管线（选号/粘性/熔断/流式/计费全部不重写），再把响应翻译回 Anthropic 形态。
// 这样协议差异只集中在两个纯函数 + 一个流式转写器里，核心链路零改动。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// anthropicDefaultModel 请求里的模型名不是网关模型时用的兜底（Claude Code 会发
// claude-sonnet-4-5 这类名字，网关没有这些模型，统一落到 auto）。
const anthropicDefaultModel = "cn:auto"

// ---------------------------------------------------------------- 请求结构

type anthropicRequest struct {
	Model       string          `json:"model"`
	Messages    []anthropicMsg  `json:"messages"`
	System      json.RawMessage `json:"system"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	StopSeqs    []string        `json:"stop_sequences"`
	Stream      bool            `json:"stream"`
	Tools       []anthropicTool `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Thinking    json.RawMessage `json:"thinking"`
	Metadata    map[string]any  `json:"metadata"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *anthropicImage `json:"source"`
}

type anthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// ---------------------------------------------------------------- OpenAI 结构（仅本文件用）

type oaToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaMessage struct {
	Role       string          `json:"role"`
	Content    any             `json:"content,omitempty"`
	ToolCalls  []oaToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// ---------------------------------------------------------------- 请求转换

// anthropicToOpenAI 把 Anthropic Messages 请求翻译成 OpenAI chat/completions 请求。
func anthropicToOpenAI(raw []byte) ([]byte, error) {
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}

	out := map[string]any{
		"model":  anthropicModelName(req.Model),
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSeqs) > 0 {
		out["stop"] = req.StopSeqs
	}
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		out["thinking"] = json.RawMessage(req.Thinking)
	}

	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, anthropicTurn(m)...)
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			// web_search 这类 server tool 网关没有对应能力，跳过而不是报错
			// （否则 Claude Code 带上搜索工具就整条请求 400）。
			if t.Name == "" || strings.HasPrefix(t.Type, "web_search") {
				continue
			}
			schema := json.RawMessage(`{"type":"object","properties":{}}`)
			if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
				schema = t.InputSchema
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  schema,
				},
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// anthropicModelName 把 Claude 的模型名映射到网关模型。
// 带 cn:/global: 前缀的原样透传；claude-* 或空名落到 auto。
func anthropicModelName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return anthropicDefaultModel
	}
	if strings.HasPrefix(name, "cn:") || strings.HasPrefix(name, "global:") {
		return name
	}
	if strings.HasPrefix(strings.ToLower(name), "claude") {
		return anthropicDefaultModel
	}
	return name
}

// anthropicSystemText 取 system 文本：既支持字符串，也支持 block 数组。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// anthropicTurn 把一条 Anthropic 消息展开成一条或多条 OpenAI 消息。
//
// tool_result 必须拆成独立的 role=tool 消息（OpenAI 的约定），
// tool_use 则挂到 assistant 的 tool_calls 上。
func anthropicTurn(m anthropicMsg) []oaMessage {
	var plain string
	if err := json.Unmarshal(m.Content, &plain); err == nil {
		return []oaMessage{{Role: m.Role, Content: plain}}
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}

	out := make([]oaMessage, 0, len(blocks))
	assistant := oaMessage{Role: "assistant"}
	var assistantText []string
	var assistantParts []map[string]any
	var calls []oaToolCall

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			assistantText = append(assistantText, b.Text)
		case "image":
			if b.Source == nil {
				continue
			}
			url := b.Source.URL
			if url == "" && b.Source.Data != "" {
				url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
			}
			if url != "" {
				assistantParts = append(assistantParts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			calls = append(calls, oaToolCall{
				ID:   b.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: b.Name, Arguments: args},
			})
		case "tool_result":
			content := toolResultText(b.Content)
			out = append(out, oaMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    content,
			})
		}
	}

	if m.Role == "assistant" {
		if len(calls) > 0 {
			assistant.ToolCalls = calls
		}
		if len(assistantParts) > 0 {
			parts := make([]map[string]any, 0, len(assistantParts)+1)
			if len(assistantText) > 0 {
				parts = append(parts, map[string]any{
					"type": "text",
					"text": strings.Join(assistantText, "\n"),
				})
			}
			parts = append(parts, assistantParts...)
			assistant.Content = parts
		} else if len(assistantText) > 0 {
			assistant.Content = strings.Join(assistantText, "\n")
		}
		if assistant.Content != nil || len(assistant.ToolCalls) > 0 {
			out = append([]oaMessage{assistant}, out...)
		}
		return out
	}

	if len(assistantText) > 0 {
		if len(assistantParts) > 0 {
			parts := []map[string]any{{"type": "text", "text": strings.Join(assistantText, "\n")}}
			parts = append(parts, assistantParts...)
			out = append([]oaMessage{{Role: m.Role, Content: parts}}, out...)
		} else {
			out = append([]oaMessage{{Role: m.Role, Content: strings.Join(assistantText, "\n")}}, out...)
		}
	} else if len(assistantParts) > 0 {
		out = append([]oaMessage{{Role: m.Role, Content: assistantParts}}, out...)
	}
	return out
}

// toolResultText 取 tool_result 的文本（可能是字符串，也可能是 block 数组）。
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicToolChoice 映射 tool_choice。
func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "any":
			return "required"
		default:
			return s
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	switch obj.Type {
	case "any":
		return "required"
	case "tool":
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": obj.Name},
		}
	default:
		return obj.Type
	}
}

// ---------------------------------------------------------------- 响应转换（非流式）

type oaResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      oaMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// openAIToAnthropic 把 OpenAI 响应翻译成 Anthropic Messages 响应。
func openAIToAnthropic(raw []byte, model string) ([]byte, error) {
	var oa oaResponse
	if err := json.Unmarshal(raw, &oa); err != nil {
		return nil, err
	}
	content := make([]map[string]any, 0, 2)
	stop := "end_turn"
	if len(oa.Choices) > 0 {
		ch := oa.Choices[0]
		if s, ok := ch.Message.Content.(string); ok && s != "" {
			content = append(content, map[string]any{"type": "text", "text": s})
		}
		for _, call := range ch.Message.ToolCalls {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    call.ID,
				"name":  call.Function.Name,
				"input": rawJSONOrEmpty(call.Function.Arguments),
			})
			stop = "tool_use"
		}
		switch ch.FinishReason {
		case "length":
			stop = "max_tokens"
		case "tool_calls", "function_call":
			stop = "tool_use"
		case "stop", "":
			if stop != "tool_use" {
				stop = "end_turn"
			}
		}
	}
	if model == "" {
		model = oa.Model
	}
	resp := map[string]any{
		"id":            "msg_" + strings.TrimPrefix(oa.ID, "chatcmpl-"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  oa.Usage.PromptTokens,
			"output_tokens": oa.Usage.CompletionTokens,
		},
	}
	return json.Marshal(resp)
}

func rawJSONOrEmpty(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(s)) {
		b, _ := json.Marshal(s)
		return b
	}
	return json.RawMessage(s)
}

// ---------------------------------------------------------------- 流式转写

// anthropicStream 把 OpenAI 的 SSE 流转写成 Anthropic 的事件流。
//
// 状态机：text 块与每个 tool_call 各占一个 content block index，
// 先发 content_block_start，增量走 content_block_delta，收尾统一 stop。
type anthropicStream struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	model     string
	buf       bytes.Buffer
	started   bool
	nextIndex int
	textIndex int // -1 表示还没开文本块
	textOpen  bool
	tools     map[int]*toolStream
	order     []int
	usage     struct{ in, out int }
	stop      string
}

type toolStream struct {
	index int
	id    string
	name  string
	open  bool
	args  strings.Builder
}

func newAnthropicStream(w http.ResponseWriter, model string) *anthropicStream {
	s := &anthropicStream{w: w, model: model, textIndex: -1, tools: map[int]*toolStream{}}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	return s
}

func (s *anthropicStream) event(name string, payload any) {
	b, _ := json.Marshal(payload)
	_, _ = io.WriteString(s.w, "event: "+name+"\ndata: ")
	_, _ = s.w.Write(b)
	_, _ = io.WriteString(s.w, "\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *anthropicStream) ensureStart() {
	if s.started {
		return
	}
	s.started = true
	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            "msg_" + randomHex(12),
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *anthropicStream) writeText(text string) {
	if text == "" {
		return
	}
	s.ensureStart()
	if !s.textOpen {
		s.textOpen = true
		s.textIndex = s.nextIndex
		s.nextIndex++
		s.event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         s.textIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	s.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (s *anthropicStream) writeToolDelta(call oaToolCall) {
	s.ensureStart()
	ts, ok := s.tools[call.Index]
	if !ok {
		ts = &toolStream{index: s.nextIndex}
		s.nextIndex++
		s.tools[call.Index] = ts
		s.order = append(s.order, call.Index)
	}
	if !ts.open {
		if call.ID != "" {
			ts.id = call.ID
		}
		if call.Function.Name != "" {
			ts.name = call.Function.Name
		}
		// 名字和 id 可能分两帧到达：等到有名字再开块
		if ts.name == "" {
			return
		}
		ts.open = true
		if ts.id == "" {
			ts.id = "toolu_" + randomHex(12)
		}
		s.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": ts.index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    ts.id,
				"name":  ts.name,
				"input": map[string]any{},
			},
		})
	}
	if call.Function.Arguments != "" {
		s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": ts.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
		})
	}
}

func (s *anthropicStream) finish() {
	s.ensureStart()
	if s.textOpen {
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
	}
	for _, k := range s.order {
		if ts := s.tools[k]; ts != nil && ts.open {
			s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": ts.index})
		}
	}
	if s.stop == "" {
		s.stop = "end_turn"
	}
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": s.usage.out},
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

// feed 解析 OpenAI 的 data: 帧。
func (s *anthropicStream) feed(frame string) {
	frame = strings.TrimSpace(frame)
	if frame == "" || strings.HasPrefix(frame, "event:") {
		return
	}
	payload := strings.TrimPrefix(frame, "data: ")
	if payload == "[DONE]" {
		return
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string       `json:"content"`
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if chunk.Usage.PromptTokens > 0 {
		s.usage.in = chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens > 0 {
		s.usage.out = chunk.Usage.CompletionTokens
	}
	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" {
			s.writeText(ch.Delta.Content)
		}
		for _, call := range ch.Delta.ToolCalls {
			s.writeToolDelta(call)
		}
		switch ch.FinishReason {
		case "length":
			s.stop = "max_tokens"
		case "tool_calls", "function_call":
			s.stop = "tool_use"
		case "stop":
			if s.stop == "" {
				s.stop = "end_turn"
			}
		}
	}
}

// ---------------------------------------------------------------- 响应写入器

// anthropicWriter 拦截 chatCompletions 的输出并转成 Anthropic 形态。
//
// 流式：按 SSE 帧转写；非流式：整体缓冲后转换；错误：OpenAI 错误体转 Anthropic 错误体。
type anthropicWriter struct {
	http.ResponseWriter
	model    string
	stream   bool
	status   int
	streamer *anthropicStream
	body     bytes.Buffer
	wrote    bool
}

func (a *anthropicWriter) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
}

func (a *anthropicWriter) Write(p []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if a.status >= 400 {
		return a.body.Write(p)
	}
	if !a.stream {
		return a.body.Write(p)
	}
	if a.streamer == nil {
		a.streamer = newAnthropicStream(a.ResponseWriter, a.model)
		a.wrote = true
	}
	a.streamer.buf.Write(p)
	for {
		data := a.streamer.buf.Bytes()
		idx := bytes.Index(data, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := string(data[:idx])
		a.streamer.buf.Next(idx + 2)
		a.streamer.feed(frame)
	}
	return len(p), nil
}

func (a *anthropicWriter) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish 在 handler 返回后调用：补齐收尾事件或转换非流式响应体。
func (a *anthropicWriter) finish() {
	if a.status >= 400 {
		a.writeError()
		return
	}
	if a.stream {
		if a.streamer != nil {
			a.streamer.finish()
		}
		return
	}
	out, err := openAIToAnthropic(a.body.Bytes(), a.model)
	if err != nil {
		a.ResponseWriter.Header().Set("Content-Type", "application/json")
		a.ResponseWriter.WriteHeader(http.StatusBadGateway)
		_, _ = a.ResponseWriter.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"bad upstream response"}}`))
		return
	}
	a.ResponseWriter.Header().Set("Content-Type", "application/json")
	a.ResponseWriter.Header().Del("Content-Length")
	a.ResponseWriter.WriteHeader(http.StatusOK)
	_, _ = a.ResponseWriter.Write(out)
}

// writeError 把 OpenAI 错误体转成 Anthropic 错误体（客户端只认后者）。
func (a *anthropicWriter) writeError() {
	var oa struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.body.Bytes(), &oa)
	msg := oa.Error.Message
	if msg == "" {
		msg = http.StatusText(a.status)
	}
	typ := "api_error"
	switch a.status {
	case http.StatusBadRequest:
		typ = "invalid_request_error"
	case http.StatusUnauthorized:
		typ = "authentication_error"
	case http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case http.StatusNotFound:
		typ = "not_found_error"
	}
	b, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
	a.ResponseWriter.Header().Set("Content-Type", "application/json")
	a.ResponseWriter.Header().Del("Content-Length")
	a.ResponseWriter.WriteHeader(a.status)
	_, _ = a.ResponseWriter.Write(b)
}

// ---------------------------------------------------------------- 入口

// anthropicMessages 处理 POST /v1/messages（Anthropic 对话协议）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(raw, &peek)

	converted, err := anthropicToOpenAI(raw)
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	aw := &anthropicWriter{
		ResponseWriter: w,
		model:          peek.Model,
		stream:         peek.Stream,
	}
	defer aw.finish()

	// 复用既有管线：换掉 body 与路径，其余（鉴权上下文、选号、计费）不变。
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/chat/completions"
	r2.Body = io.NopCloser(bytes.NewReader(converted))
	r2.ContentLength = int64(len(converted))
	r2.Header.Set("Content-Type", "application/json")
	h.chatCompletions(aw, r2)
}

// anthropicCountTokens Claude Code 会先估算 token；给个够用的近似值即可
// （客户端只用它做上下文预算，不参与计费）。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	// 粗略估算：ASCII 4 字符≈1 token，CJK 1 字符≈1 token。
	ascii, other := 0, 0
	for _, b := range raw {
		if b < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	tokens := ascii/4 + other
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// ensure bufio is used (kept for future SSE parsing needs)
var _ = bufio.NewReader
