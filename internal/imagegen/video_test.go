package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// h3OpenAPI is shaped like the OpenAPI document fal publishes for a
// reference-to-video model.
const h3OpenAPI = `{
  "openapi": "3.0.4",
  "paths": {
    "/minimax/h3-max/reference-to-video/requests/{request_id}/status": {"get": {}},
    "/minimax/h3-max/reference-to-video": {"post": {"requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/H3MaxReferenceToVideoInput"}}}}}}
  },
  "components": {"schemas": {
    "QueueStatus": {"type": "object", "properties": {"status": {"type": "string"}}},
    "DurationEnum": {"type": "string", "enum": ["5", "10", "15"]},
    "H3MaxReferenceToVideoInput": {
      "type": "object",
      "required": ["prompt"],
      "x-fal-order-properties": ["prompt", "first_frame_image_url", "last_frame_image_url", "reference_image_urls", "reference_video_urls", "reference_audio_urls", "duration", "aspect_ratio", "resolution"],
      "properties": {
        "prompt": {"type": "string", "description": "Refer to assets as Image 1, Video 1, Audio 1."},
        "first_frame_image_url": {"anyOf": [{"type": "string"}, {"type": "null"}], "description": "The video opens on this image."},
        "last_frame_image_url": {"anyOf": [{"type": "string"}, {"type": "null"}]},
        "reference_image_urls": {"type": "array", "items": {"type": "string"}},
        "reference_video_urls": {"anyOf": [{"type": "array", "items": {"type": "string"}}, {"type": "null"}]},
        "reference_audio_urls": {"type": "array", "items": {"type": "string"}},
        "duration": {"$ref": "#/components/schemas/DurationEnum"},
        "aspect_ratio": {"type": "string", "enum": ["16:9", "9:16"]},
        "resolution": {"type": "string", "enum": ["720p", "1080p"]}
      }
    }
  }}
}`

func TestParseOpenAPI(t *testing.T) {
	s, err := ParseOpenAPI("minimax/h3-max/reference-to-video", json.RawMessage(h3OpenAPI))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range s.Params {
		names = append(names, p.Name+":"+p.Type)
	}
	want := "prompt:string first_frame_image_url:string last_frame_image_url:string reference_image_urls:array " +
		"reference_video_urls:array reference_audio_urls:array duration:string aspect_ratio:string resolution:string"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("params:\n got %s\nwant %s", got, want)
	}
	if d := s.Param("duration"); !reflect.DeepEqual(d.Enum, []string{"5", "10", "15"}) {
		t.Errorf("duration enum through $ref: %+v", d)
	}
	if !s.Param("prompt").Required || s.Param("duration").Required {
		t.Error("required flags")
	}
	if _, err := ParseOpenAPI("m", json.RawMessage(`{"components":{"schemas":{}}}`)); err == nil {
		t.Error("a document without inputs should fail")
	}
}

func TestResolveRoles(t *testing.T) {
	h3, _ := ParseOpenAPI("h3", json.RawMessage(h3OpenAPI))
	names := func(m map[string]Param) map[string]string {
		out := map[string]string{}
		for role, p := range m {
			out[role] = p.Name
		}
		return out
	}
	if got, want := names(ResolveRoles(h3, nil)), map[string]string{
		RolePrompt: "prompt", RoleFirstFrame: "first_frame_image_url", RoleLastFrame: "last_frame_image_url",
		RoleRefImages: "reference_image_urls", RoleRefVideos: "reference_video_urls", RoleRefAudios: "reference_audio_urls",
		RoleDuration: "duration", RoleAspect: "aspect_ratio",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("h3 roles: %v", got)
	}
	// A plain image-to-video model: image_url is the first frame, and an
	// end image is recognised under another name.
	kling := &InputSchema{Params: []Param{
		{Name: "prompt", Type: "string"}, {Name: "image_url", Type: "string"}, {Name: "tail_image_url", Type: "string"},
		{Name: "duration", Type: "string", Enum: []string{"5", "10"}},
	}}
	if got, want := names(ResolveRoles(kling, nil)), map[string]string{
		RolePrompt: "prompt", RoleFirstFrame: "image_url", RoleLastFrame: "tail_image_url", RoleDuration: "duration",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("kling roles: %v", got)
	}
	// Unknown schema: only the near-universal names, plus overrides.
	got := names(ResolveRoles(nil, map[string]string{RoleLastFrame: "end_image_url", RoleRefVideos: "video_urls", "bogus": "x"}))
	if want := map[string]string{RolePrompt: "prompt", RoleFirstFrame: "image_url", RoleLastFrame: "end_image_url", RoleRefVideos: "video_urls"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fallback roles: %v", got)
	}
	// An override can also switch a role off.
	if _, ok := ResolveRoles(h3, map[string]string{RoleLastFrame: "-"})[RoleLastFrame]; ok {
		t.Error("override '-' should remove the role")
	}
}

func TestDurationValue(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		p    Param
		secs float64
		want any
	}{
		{Param{Type: "string", Enum: []string{"5", "10"}}, 6.4, "5"},
		{Param{Type: "string", Enum: []string{"4s", "6s", "8s"}}, 7.2, "8s"},
		{Param{Type: "integer", Enum: []string{"5", "10"}}, 9, float64(10)},
		{Param{Type: "integer", Min: f(3), Max: f(12)}, 20, 12},
		{Param{Type: "number"}, 4.5, 4.5},
		{Param{Type: "string"}, 4.6, "5"},
	}
	for _, c := range cases {
		if got := durationValue(c.p, c.secs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("durationValue(%+v, %v) = %#v, want %#v", c.p, c.secs, got, c.want)
		}
	}
}

func TestVideoPayload(t *testing.T) {
	h3, _ := ParseOpenAPI("h3", json.RawMessage(h3OpenAPI))
	img := func(n string) Image { return Image{URL: "https://x/" + n} }
	first, last := img("first.png"), img("last.png")
	req := &Request{
		Kind: KindVideo, Model: "h3", Prompt: "Image 1 walks in", AspectRatio: "9:16", Duration: 6, Schema: h3,
		FirstFrame: &first, LastFrame: &last, References: []Image{img("r1.png"), img("r2.png")},
		RefVideos: []Image{img("v.mp4")}, RefAudios: []Image{{MIME: "audio/mpeg", Data: []byte("abc")}},
		Extra: map[string]any{"resolution": "1080p"},
	}
	g, _ := New(Options{Type: TypeFal})
	got := g.(*fal).payload(req)
	want := map[string]any{
		"prompt": "Image 1 walks in", "first_frame_image_url": "https://x/first.png", "last_frame_image_url": "https://x/last.png",
		"reference_image_urls": []string{"https://x/r1.png", "https://x/r2.png"}, "reference_video_urls": []string{"https://x/v.mp4"},
		"reference_audio_urls": []string{"data:audio/mpeg;base64,YWJj"}, "duration": "5", "aspect_ratio": "9:16", "resolution": "1080p",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload:\n got %v\nwant %v", got, want)
	}
	notes := strings.Join(g.(Explainer).Explain(req), "; ")
	for _, w := range []string{"first frame → first_frame_image_url", "last frame → last_frame_image_url", "reference audio → reference_audio_urls", "duration → duration = 5"} {
		if !strings.Contains(notes, w) {
			t.Errorf("notes %q missing %q", notes, w)
		}
	}

	// A model with fewer inputs: what cannot be sent is reported, not
	// silently dropped, and an unsupported aspect ratio is left out.
	simple := &InputSchema{Params: []Param{
		{Name: "prompt", Type: "string"}, {Name: "image_url", Type: "string"},
		{Name: "aspect_ratio", Type: "string", Enum: []string{"16:9"}},
	}}
	req.Schema, req.Extra = simple, nil
	got = g.(*fal).payload(req)
	if want := map[string]any{"prompt": "Image 1 walks in", "image_url": "https://x/first.png"}; !reflect.DeepEqual(got, want) {
		t.Errorf("simple payload: %v", got)
	}
	notes = strings.Join(g.(Explainer).Explain(req), "; ")
	for _, w := range []string{"last frame: not supported", "reference videos: not supported", "aspect ratio: 9:16 is not offered"} {
		if !strings.Contains(notes, w) {
			t.Errorf("notes %q missing %q", notes, w)
		}
	}
	// Without a schema, say so.
	req.Schema = nil
	if notes := g.(Explainer).Explain(req); !strings.Contains(notes[0], "parameter list is unknown") {
		t.Errorf("no-schema notes: %v", notes)
	}
}

func TestFalVideoJob(t *testing.T) {
	var polls int
	var cancelled bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/acme/video":
			fmt.Fprintf(w, `{"request_id":"v1","status_url":"%[1]s/acme/requests/v1/status","response_url":"%[1]s/acme/requests/v1"}`, srv.URL)
		case r.URL.Path == "/acme/requests/v1/status":
			polls++
			switch polls {
			case 1:
				w.Write([]byte(`{"status":"IN_QUEUE","queue_position":2}`))
			case 2, 3:
				w.Write([]byte(`{"status":"IN_PROGRESS"}`))
			default:
				w.Write([]byte(`{"status":"COMPLETED"}`))
			}
		case r.URL.Path == "/acme/requests/v1":
			fmt.Fprintf(w, `{"video":{"url":"%s/files/out.mp4","content_type":"video/mp4"},"seed":7}`, srv.URL)
		case r.URL.Path == "/files/out.mp4":
			w.Write([]byte("mp4 bytes"))
		case r.Method == http.MethodPut && r.URL.Path == "/acme/requests/v1/cancel":
			cancelled = true
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/acme/requests/gone/status":
			http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
		case r.URL.Path == "/acme/requests/busy/status":
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g, _ := New(Options{Type: TypeFal, BaseURL: srv.URL, PollInterval: time.Millisecond})
	q := g.(Queued)
	ctx := context.Background()

	job, err := q.Submit(ctx, &Request{Kind: KindVideo, Model: "acme/video", Prompt: "p"})
	if err != nil || job.RequestID != "v1" {
		t.Fatalf("submit: %+v %v", job, err)
	}
	// The job is plain data: a copy (as if reloaded from disk) can be waited on.
	b, _ := json.Marshal(job)
	var reloaded Job
	json.Unmarshal(b, &reloaded)
	var states []string
	res, err := q.Wait(ctx, &reloaded, func(s string) { states = append(states, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Videos) != 1 || res.Videos[0].MIME != "video/mp4" || string(res.Videos[0].Data) != "mp4 bytes" || len(res.Images) != 0 {
		t.Errorf("result: %+v", res)
	}
	if got := strings.Join(states, ","); got != "IN_QUEUE (position 2),IN_PROGRESS" {
		t.Errorf("states: %s", got)
	}
	if err := q.Cancel(ctx, job); err != nil || !cancelled {
		t.Errorf("cancel: %v %v", err, cancelled)
	}

	// Errors: a vanished job is final, an overloaded server is not.
	_, err = q.Wait(ctx, &Job{StatusURL: srv.URL + "/acme/requests/gone/status", ResponseURL: "x"}, nil)
	if err == nil || Retryable(err) {
		t.Errorf("404 should be final: %v", err)
	}
	_, err = q.Wait(ctx, &Job{StatusURL: srv.URL + "/acme/requests/busy/status", ResponseURL: "x"}, nil)
	if !Retryable(err) {
		t.Errorf("503 should be retryable: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = q.Wait(cctx, job, nil); !Retryable(err) {
		t.Errorf("cancellation should be retryable: %v", err)
	}
	if Retryable(errors.New("fal: result has no images or video")) || Retryable(nil) {
		t.Error("plain errors are final")
	}
}

func TestFalModelSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1/models" || q.Get("expand") != "openapi-3.0" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"models":[{"endpoint_id":%q,"metadata":{"category":"image-to-video"},"openapi":%s}]}`, "minimax/h3-max/reference-to-video", h3OpenAPI)
	}))
	defer srv.Close()
	o := Options{Type: TypeFal, CatalogURL: srv.URL + "/v1"}
	s, err := ModelSchema(context.Background(), o, "minimax/h3-max/reference-to-video")
	if err != nil || s.Param("last_frame_image_url") == nil {
		t.Fatalf("schema: %+v %v", s, err)
	}
	if _, err := ModelSchema(context.Background(), o, "other/model"); err == nil {
		t.Error("a model that is not in the reply should fail")
	}
}
