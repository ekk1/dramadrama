// Package imagegen defines a provider-neutral image generation interface.
// fal.ai is the first implementation; others can be added behind the same
// interface.
package imagegen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Image is an input or output image. Inputs need Data+MIME (or a public
// URL); outputs always have Data downloaded.
type Image struct {
	MIME   string
	Data   []byte
	URL    string
	Width  int
	Height int
}

// Request is one image generation call.
type Request struct {
	Model          string
	Prompt         string
	NegativePrompt string
	// AspectRatio such as "9:16", "16:9", "1:1".
	AspectRatio string
	NumImages   int
	Seed        *int64
	// References are conditioning images (style/character/scene refs).
	References []Image
	// Extra is merged into the provider payload last, so it can override
	// anything.
	Extra map[string]any
}

// Result of a generation call.
type Result struct {
	Images    []Image
	Seed      *int64
	RequestID string
	Raw       json.RawMessage
}

// Generator is implemented by every image backend.
type Generator interface {
	Generate(ctx context.Context, req *Request) (*Result, error)
}

// PayloadDescriber is implemented by generators that can show the request
// body they would send, for the request log. Inline binary data is
// replaced by a short placeholder and credentials are never included.
type PayloadDescriber interface {
	DescribePayload(req *Request) map[string]any
}

// Provider types.
const TypeFal = "fal"

// Types lists the supported image provider types.
var Types = []string{TypeFal}

// Options configures a generator.
type Options struct {
	Type       string
	BaseURL    string
	APIKey     string
	Headers    map[string]string
	HTTPClient *http.Client
	// PollInterval for queue-based providers; default 1s.
	PollInterval time.Duration
}

// New constructs a generator.
func New(o Options) (Generator, error) {
	switch o.Type {
	case TypeFal:
		return newFal(o), nil
	}
	return nil, fmt.Errorf("imagegen: unknown provider type %q", o.Type)
}
