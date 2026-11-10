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

// Image is an input or output media file: an image, or for video requests
// a video or audio clip. Inputs need Data+MIME (or a public URL); outputs
// always have Data downloaded.
type Image struct {
	MIME   string
	Data   []byte
	URL    string
	Width  int
	Height int
}

// Request kinds.
const (
	KindImage = "image"
	KindVideo = "video"
)

// Request is one generation call.
type Request struct {
	// Kind is KindImage (the default) or KindVideo.
	Kind           string
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

	// Video inputs. Which payload field carries each of them depends on
	// the model: see ResolveRoles.
	FirstFrame *Image
	LastFrame  *Image
	RefVideos  []Image
	RefAudios  []Image
	Duration   float64 // seconds; 0 leaves the model's default
	// Schema is the model's declared inputs, if known.
	Schema *InputSchema
	// Mapping overrides the detected role → field assignment.
	Mapping map[string]string
}

// Result of a generation call.
type Result struct {
	Images    []Image
	Videos    []Image
	Seed      *int64
	RequestID string
	Raw       json.RawMessage
}

// Generator is implemented by every backend.
type Generator interface {
	Generate(ctx context.Context, req *Request) (*Result, error)
}

// Job identifies a request that a queue-based provider has accepted. It is
// all that is needed to collect the result later, even from another
// process, so callers persist it while the request is in flight.
type Job struct {
	RequestID   string `json:"request_id"`
	StatusURL   string `json:"status_url"`
	ResponseURL string `json:"response_url"`
}

// Queued is implemented by generators whose requests can be submitted and
// collected separately. Generate is Submit followed by Wait.
type Queued interface {
	Submit(ctx context.Context, req *Request) (*Job, error)
	// Wait polls until the job finishes and downloads its outputs.
	// onStatus, if not nil, is called whenever the reported state changes.
	Wait(ctx context.Context, job *Job, onStatus func(string)) (*Result, error)
	// Cancel asks the provider to drop a job. Best effort.
	Cancel(ctx context.Context, job *Job) error
}

// Explainer is implemented by generators that can say, in plain English,
// how a video request's inputs map onto the model's fields and which ones
// the model cannot take.
type Explainer interface {
	Explain(req *Request) []string
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
	// CatalogURL overrides the endpoint used to list models, where that is
	// a different host from BaseURL (fal).
	CatalogURL string
}

// New constructs a generator.
func New(o Options) (Generator, error) {
	switch o.Type {
	case TypeFal:
		return newFal(o), nil
	}
	return nil, fmt.Errorf("imagegen: unknown provider type %q", o.Type)
}
