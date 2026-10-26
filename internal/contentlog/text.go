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
