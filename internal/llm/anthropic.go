package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
)

// anthropicVersion is the API version header sent by default. It can be
// overridden per provider via the "anthropic-version" header in config.
const anthropicVersion = "2026-10-21"

// Default output caps when the request does not set one. Streaming can
// afford a much larger cap because it is not subject to HTTP timeouts.
const (
	anthropicDefaultMaxTokens       = 16000
	anthropicDefaultStreamMaxTokens = 64000
)

// anthropic implements the Anthropic Messages API (POST {base}/v1/messages).
//
// Sampling parameters (temperature, top_p, top_k) are never sent: API
// version 2026-10-21 rejects them on every model.
type anthropic struct{ base }

type anMessage struct {
	Role    string   `json:"role"`
	Content []anPart `json:"content"`
}

type anPart struct {
	Type   string    `json:"type"`
	Text   string    `json:"text,omitempty"`
	Source *anSource `json:"source,omitempty"`
}

type anSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anRequest struct {
	Model     string      `json:"model"`
	System    string      `json:"system,omitempty"`
	Messages  []anMessage `json:"messages"`
	MaxTokens int         `json:"max_tokens"`
	Stream    bool        `json:"stream,omitempty"`
}

type anUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (c *anthropic) build(req *Request, stream bool) *anRequest {
	out := &anRequest{Model: req.Model, System: req.System, MaxTokens: req.MaxTokens, Stream: stream}
	if out.MaxTokens <= 0 {
		out.MaxTokens = anthropicDefaultMaxTokens
		if stream {
			out.MaxTokens = anthropicDefaultStreamMaxTokens
		}
	}
	for _, m := range req.Messages {
		am := anMessage{Role: string(m.Role)}
		for _, p := range m.Content {
			switch p.Type {
			case PartText:
				am.Content = append(am.Content, anPart{Type: "text", Text: p.Text})
			case PartImage:
				am.Content = append(am.Content, anPart{Type: "image", Source: &anSource{
					Type: "base64", MediaType: p.MIME, Data: base64.StdEncoding.EncodeToString(p.Data),
				}})
			}
		}
		out.Messages = append(out.Messages, am)
	}
	return out
}

func (c *anthropic) headers() map[string]string {
	h := map[string]string{"anthropic-version": anthropicVersion}
	if c.apiKey != "" {
		h["x-api-key"] = c.apiKey
	}
	return h
}

func (c *anthropic) Chat(ctx context.Context, req *Request) (*Response, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string  `json:"stop_reason"`
		Usage      anUsage `json:"usage"`
	}
	if err := c.postJSON(ctx, c.baseURL+"/v1/messages", c.headers(), c.build(req, false), &out); err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" { // thinking blocks are skipped
			sb.WriteString(b.Text)
		}
	}
	if out.StopReason == "refusal" && sb.Len() == 0 {
		return nil, fmt.Errorf("llm: anthropic: request was declined (stop_reason=refusal)")
	}
	return &Response{
		Text:         sb.String(),
		FinishReason: out.StopReason,
		Usage:        Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens},
	}, nil
}

func (c *anthropic) Stream(ctx context.Context, req *Request) iter.Seq2[Chunk, error] {
	var inputTokens int
	return c.streamSSE(ctx, c.baseURL+"/v1/messages", c.headers(), c.build(req, true),
		func(ev sseEvent, yield func(Chunk) bool) (bool, error) {
			if ev.Data == "" {
				return true, nil
			}
			var d struct {
				Type    string `json:"type"`
				Message *struct {
					Usage anUsage `json:"usage"`
				} `json:"message"`
				Delta *struct {
					Type       string `json:"type"`
					Text       string `json:"text"`
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage *anUsage `json:"usage"`
				Error *struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
				return false, fmt.Errorf("llm: anthropic: bad stream event: %w", err)
			}
			switch d.Type {
			case "message_start":
				if d.Message != nil {
					inputTokens = d.Message.Usage.InputTokens
				}
			case "content_block_delta":
				// Only text deltas are surfaced; thinking/signature deltas are ignored.
				if d.Delta != nil && d.Delta.Type == "text_delta" && d.Delta.Text != "" {
					return yield(Chunk{Delta: d.Delta.Text}), nil
				}
			case "message_delta":
				ch := Chunk{}
				if d.Delta != nil {
					ch.FinishReason = d.Delta.StopReason
				}
				if d.Usage != nil {
					ch.Usage = &Usage{InputTokens: inputTokens, OutputTokens: d.Usage.OutputTokens}
				}
				if ch.FinishReason == "refusal" {
					return false, fmt.Errorf("llm: anthropic: request was declined (stop_reason=refusal)")
				}
				return yield(ch), nil
			case "message_stop":
				return false, nil
			case "error":
				msg := "stream error"
				if d.Error != nil {
					msg = d.Error.Type + ": " + d.Error.Message
				}
				return false, fmt.Errorf("llm: anthropic: %s", msg)
			}
			return true, nil
		})
}
