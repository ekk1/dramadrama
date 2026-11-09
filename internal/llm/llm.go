// Package llm provides a single, provider-neutral client interface for chat
// style language models, with implementations for the OpenAI Chat Completions
// API, the OpenAI Responses API, the Anthropic Messages API and the Google
// Gemini API. Every implementation supports both a blocking call and a
// streaming call.
package llm

import (
	"context"
	"encoding/base64"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"
)

// Role is the author of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// PartType distinguishes content parts.
type PartType string

const (
	PartText  PartType = "text"
	PartImage PartType = "image"
)

// Part is one piece of message content: text or an inline image.
type Part struct {
	Type PartType
	Text string
	MIME string // for images, e.g. "image/png"
	Data []byte // raw image bytes
}

// Message is one turn of a conversation.
type Message struct {
	Role    Role
	Content []Part
}

// TextPart returns a text content part.
func TextPart(s string) Part { return Part{Type: PartText, Text: s} }

// ImagePart returns an inline image content part.
func ImagePart(mime string, data []byte) Part {
	return Part{Type: PartImage, MIME: mime, Data: data}
}

// UserText is a convenience constructor for a plain-text user message.
func UserText(s string) Message { return Message{Role: RoleUser, Content: []Part{TextPart(s)}} }

// Request is a provider-neutral generation request.
type Request struct {
	Model    string
	System   string
	Messages []Message
	// Temperature is optional. It is ignored by providers that no longer
	// accept sampling parameters (Anthropic).
	Temperature *float64
	// MaxTokens caps the output length. Zero means provider default.
	MaxTokens int
	// JSON asks the provider for a JSON object response where the API
	// supports it natively. The prompt should still ask for JSON.
	JSON bool
}

// Usage reports token counts when the provider returns them.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the result of a blocking call, or of a collected stream.
type Response struct {
	Text         string
	FinishReason string
	Usage        Usage
}

// Chunk is one streaming event. Delta carries new text; the final chunk of a
// stream may carry only FinishReason and/or Usage.
type Chunk struct {
	Delta        string
	FinishReason string
	Usage        *Usage
}

// Client is implemented by every provider.
type Client interface {
	// Chat performs a blocking request and returns the full response.
	Chat(ctx context.Context, req *Request) (*Response, error)
	// Stream performs a streaming request. Iteration yields chunks as they
	// arrive; a non-nil error ends the sequence. Breaking out of the loop
	// closes the underlying connection.
	Stream(ctx context.Context, req *Request) iter.Seq2[Chunk, error]
}

// Collect drains a stream into a Response, calling onDelta (if non-nil) for
// each piece of text as it arrives.
func Collect(seq iter.Seq2[Chunk, error], onDelta func(string)) (*Response, error) {
	var sb strings.Builder
	resp := &Response{}
	for c, err := range seq {
		if err != nil {
			resp.Text = sb.String()
			return resp, err
		}
		if c.Delta != "" {
			sb.WriteString(c.Delta)
			if onDelta != nil {
				onDelta(c.Delta)
			}
		}
		if c.FinishReason != "" {
			resp.FinishReason = c.FinishReason
		}
		if c.Usage != nil {
			if c.Usage.InputTokens > 0 {
				resp.Usage.InputTokens = c.Usage.InputTokens
			}
			if c.Usage.OutputTokens > 0 {
				resp.Usage.OutputTokens = c.Usage.OutputTokens
			}
		}
	}
	resp.Text = sb.String()
	return resp, nil
}

// API types understood by New.
const (
	TypeOpenAIChat      = "openai_chat"
	TypeOpenAIResponses = "openai_responses"
	TypeAnthropic       = "anthropic"
	TypeGemini          = "gemini"
)

// Types lists the supported provider types.
var Types = []string{TypeOpenAIChat, TypeOpenAIResponses, TypeAnthropic, TypeGemini}

// Options configures a client.
type Options struct {
	Type    string
	BaseURL string // empty means the provider's public endpoint
	APIKey  string
	// Headers are added to every request (and override defaults such as
	// anthropic-version).
	Headers    map[string]string
	HTTPClient *http.Client
}

// New constructs a client for the given provider type.
func New(o Options) (Client, error) {
	hc := o.HTTPClient
	if hc == nil {
		// No overall timeout: streams can legitimately run for minutes.
		// Cancellation is driven by the request context.
		hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}}
	}
	b := base{http: hc, apiKey: o.APIKey, headers: o.Headers}
	switch o.Type {
	case TypeOpenAIChat:
		b.baseURL = orDefault(o.BaseURL, "https://api.openai.com/v1")
		return &openAIChat{b}, nil
	case TypeOpenAIResponses:
		b.baseURL = orDefault(o.BaseURL, "https://api.openai.com/v1")
		return &openAIResponses{b}, nil
	case TypeAnthropic:
		b.baseURL = orDefault(o.BaseURL, "https://api.anthropic.com")
		return &anthropic{b}, nil
	case TypeGemini:
		b.baseURL = orDefault(o.BaseURL, "https://generativelanguage.googleapis.com/v1beta")
		return &gemini{b}, nil
	}
	return nil, fmt.Errorf("llm: unknown provider type %q", o.Type)
}

func orDefault(s, def string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return def
	}
	return s
}

func dataURI(p Part) string {
	return "data:" + p.MIME + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}
