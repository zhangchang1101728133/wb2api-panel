package contentlog

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ExtractText 从网关写出的响应里抽出助手正文，供面板直接阅读。
//
// 支持两种形态：
//   - 非流式：OpenAI 兼容 JSON（choices[0].message.content）；
//   - 流式：SSE，逐帧取 choices[0].delta.content 并拼接。
//
// content 既可能是字符串，也可能是 [{type:"text",text:"…"}] 数组（部分客户端/模型
// 会这样回）。两种都处理；解析失败返回空串——正文抽取只是展示便利，绝不能
// 影响归档本身。
func ExtractText(raw []byte, stream bool) string {
	if len(raw) == 0 {
		return ""
	}
	if stream {
		return extractStreamText(raw)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		return ""
	}
	return contentText(resp.Choices[0].Message.Content)
}

func extractStreamText(raw []byte) string {
	var b strings.Builder
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &frame) != nil || len(frame.Choices) == 0 {
			continue
		}
		b.WriteString(contentText(frame.Choices[0].Delta.Content))
	}
	return b.String()
}

// ExtractUsage 从响应体里抽 token 用量。
//
// 归档发生在请求收尾，reqlog 事件里的 token 数可能还没回填；而响应体本身
// 一定带着 usage（非流式在顶层，流式在最后一帧）。这里当兜底来源，面板上
// 才能看到「这条对话花了多少」。
func ExtractUsage(raw []byte, stream bool) (prompt, completion int64) {
	if len(raw) == 0 {
		return 0, 0
	}
	type usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	}
	if !stream {
		var resp struct {
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(raw, &resp) == nil && resp.Usage != nil {
			return resp.Usage.PromptTokens, resp.Usage.CompletionTokens
		}
		return 0, 0
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var frame struct {
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(payload, &frame) == nil && frame.Usage != nil &&
			(frame.Usage.PromptTokens > 0 || frame.Usage.CompletionTokens > 0) {
			prompt, completion = frame.Usage.PromptTokens, frame.Usage.CompletionTokens
		}
	}
	return prompt, completion
}

// contentText 把 content 字段（字符串 / 文本块数组）摊平成纯文本。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}
