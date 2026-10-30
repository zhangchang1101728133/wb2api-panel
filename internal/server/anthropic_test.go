package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAnthropicToOpenAIBasic(t *testing.T) {
	in := `{
		"model":"claude-sonnet-4-5",
		"system":"you are helpful",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file body"}]}
		],
		"tools":[{"name":"read","description":"read file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"tool_choice":{"type":"auto"}
	}`
	out, err := anthropicToOpenAI([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "cn:auto" {
		t.Fatalf("claude 模型应映射到 cn:auto, got %v", got["model"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("应展开成 4 条消息（system/user/assistant/tool）, got %d: %s", len(msgs), out)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are helpful" {
		t.Fatalf("system 消息不对: %v", first)
	}
	asst := msgs[2].(map[string]any)
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant 应带 1 个 tool_call: %v", asst)
	}
	last := msgs[3].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "toolu_1" || last["content"] != "file body" {
		t.Fatalf("tool 消息不对: %v", last)
	}
	tools := got["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Fatalf("tools 映射不对: %v", tools)
	}
}

func TestAnthropicModelPassThrough(t *testing.T) {
	if anthropicModelName("cn:hy3") != "cn:hy3" {
		t.Fatal("cn: 前缀应透传")
	}
	if anthropicModelName("global:gpt-6-astra") != "global:gpt-6-astra" {
		t.Fatal("global: 前缀应透传")
	}
	if anthropicModelName("") != anthropicDefaultModel {
		t.Fatal("空模型应落默认")
	}
	if anthropicModelName("Claude-Opus") != anthropicDefaultModel {
		t.Fatal("claude 模型应落默认")
	}
}

func TestOpenAIToAnthropic(t *testing.T) {
	in := `{"id":"chatcmpl-abc","model":"hy3","choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`
	out, err := openAIToAnthropic([]byte(in), "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "message" || got["role"] != "assistant" {
		t.Fatalf("响应形态不对: %s", out)
	}
	if got["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason 应 end_turn: %v", got["stop_reason"])
	}
	blocks := got["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "hi there" {
		t.Fatalf("content 不对: %v", blocks)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 11 {
		t.Fatalf("usage 不对: %v", usage)
	}
}

func TestStreamTranslation(t *testing.T) {
	rec := &fakeWriter{}
	s := newAnthropicStream(rec, "hy3")
	s.feed(`data: {"choices":[{"delta":{"content":"Hel"}}]}`)
	s.feed(`data: {"choices":[{"delta":{"content":"lo"}}]}`)
	s.feed(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`)
	s.feed(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`)
	s.feed(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	s.feed(`data: [DONE]`)
	s.finish()

	out := rec.body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		`"type":"tool_use"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q\n输出:\n%s", want, out)
		}
	}
}

type fakeWriter struct {
	header http.Header
	body   strings.Builder
	status int
}

func (f *fakeWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *fakeWriter) Write(p []byte) (int, error) { return f.body.Write(p) }
func (f *fakeWriter) WriteHeader(int)             {}
