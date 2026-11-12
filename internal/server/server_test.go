package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"iter"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/catalog"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/pipeline"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

type fakeLLM struct{}

func (fakeLLM) reply(req *llm.Request) string {
	switch {
	case strings.Contains(req.System, "art director"):
		return `{"style":"film look","items":[{"kind":"character","name":"Lin","description":"heroine","prompt":"woman in red"}]}`
	case strings.Contains(req.System, "storyboard director"):
		return `{"shots":[{"scene":"Shop","characters":["Lin"],"action":"opens <box>","camera":"close","duration":3}]}`
	case strings.Contains(req.System, "cinematographer"):
		return `{"storyboard_prompt":"sb","keyframe_prompt":"kf","motion_prompt":"mv"}`
	}
	return "A story about <Lin> & time."
}

func (f fakeLLM) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	return &llm.Response{Text: f.reply(req)}, nil
}

func (f fakeLLM) Stream(ctx context.Context, req *llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{Delta: f.reply(req)}, nil) }
}

type fakeImage struct{}

func (fakeImage) Generate(ctx context.Context, req *imagegen.Request) (*imagegen.Result, error) {
	if req.Kind == imagegen.KindVideo {
		return &imagegen.Result{Videos: []imagegen.Image{{MIME: "video/mp4", Data: []byte("mp4:" + req.Prompt)}}}, nil
	}
	return &imagegen.Result{Images: []imagegen.Image{{MIME: "image/png", Data: []byte("png:" + req.Prompt)}}}, nil
}

type harness struct {
	t   *testing.T
	s   *Server
	srv *httptest.Server
	c   *http.Client
	ps  *project.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "test") // providers are faked
	t.Setenv("FAL_KEY", "test")
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := project.Open(filepath.Join(dir, "projects"))
	lib, _ := assets.Open(filepath.Join(dir, "assets"))
	prompts := &pipeline.Prompts{OverrideDir: filepath.Join(dir, "prompts")}
	pipe := pipeline.New(cfg, ps, lib, prompts)
	logs, _ := reqlog.Open(filepath.Join(dir, "logs"))
	pipe.Log = logs
	models, _ := catalog.Open(filepath.Join(dir, "models"))
	pipe.Models = models
	pipe.NewLLM = func(*config.Provider) (llm.Client, error) { return fakeLLM{}, nil }
	pipe.NewImage = func(*config.Provider) (imagegen.Generator, error) { return fakeImage{}, nil }
	h, err := New(Options{Config: cfg, Projects: ps, Assets: lib, Pipeline: pipe, Prompts: prompts, Logs: logs, Models: models})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, s: h, srv: srv, c: &http.Client{Jar: jar}, ps: ps}
}

func (h *harness) get(path string) string {
	h.t.Helper()
	resp, err := h.c.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return string(body)
}

// projectID reads the project out of a project page URL.
func projectID(u *url.URL) string {
	return strings.Split(strings.TrimPrefix(u.Path, "/projects/"), "/")[0]
}

// project returns every step page of a project, concatenated.
func (h *harness) project(pid string) string {
	h.t.Helper()
	var b strings.Builder
	for n := 1; n <= 7; n++ {
		b.WriteString(h.get(fmt.Sprintf("/projects/%s/step/%d", pid, n)))
	}
	return b.String()
}

// post submits a form and returns the final URL after redirects.
func (h *harness) post(path string, form url.Values) *url.URL {
	h.t.Helper()
	u := h.postErr(path, form)
	if e := u.Query().Get("err"); e != "" {
		h.t.Fatalf("POST %s: error %s", path, e)
	}
	return u
}

// postErr is post for requests that are expected to be rejected: the
// error comes back in the redirect's "err" parameter.
func (h *harness) postErr(path string, form url.Values) *url.URL {
	h.t.Helper()
	resp, err := h.c.PostForm(h.srv.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("POST %s: %d %s", path, resp.StatusCode, body)
	}
	return resp.Request.URL
}

// run executes a step via SSE and returns the event stream text.
func (h *harness) run(pid, stage, target string) string {
	h.t.Helper()
	resp, err := h.c.PostForm(h.srv.URL+"/projects/"+pid+"/run/"+stage, url.Values{"target": {target}})
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		h.t.Fatalf("run %s: not an event stream: %s", stage, body)
	}
	if !strings.Contains(string(body), "event: done") {
		h.t.Fatalf("run %s did not finish: %s", stage, body)
	}
	return string(body)
}

// upload posts one file to a project list and returns the final URL.
func (h *harness) upload(pid, target, name string, data []byte) *url.URL {
	h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("target", target)
	fw, _ := mw.CreateFormFile("files", name)
	fw.Write(data)
	mw.Close()
	resp, err := h.c.Post(h.srv.URL+"/projects/"+pid+"/upload", mw.FormDataContentType(), &body)
	if err != nil || resp.StatusCode != 200 {
		h.t.Fatalf("upload: %v", err)
	}
	resp.Body.Close()
	return resp.Request.URL
}

func pngFile(t *testing.T) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	return buf.Bytes()
}

func TestWorkbenchFlow(t *testing.T) {
	h := newHarness(t)
	h.get("/projects")
	u := h.post("/projects", url.Values{"name": {"Demo"}, "idea": {"time machine"}})
	pid := projectID(u)
	if !strings.HasPrefix(pid, "p_") {
		t.Fatalf("unexpected redirect %s", u)
	}
	h.post("/projects/"+pid+"/input", url.Values{"name": {"Demo 2"}, "idea": {"a girl & a <clock>"}, "aspect_ratio": {"16:9"}})

	// Upload a reference image.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("target", "project")
	fw, _ := mw.CreateFormFile("files", "ref.png")
	fw.Write(pngFile(t))
	mw.Close()
	resp, err := h.c.Post(h.srv.URL+"/projects/"+pid+"/upload", mw.FormDataContentType(), &body)
	if err != nil || resp.StatusCode != 200 || resp.Request.URL.Query().Get("err") != "" {
		t.Fatalf("upload: %v %v", err, resp.Request.URL)
	}
	resp.Body.Close()

	stream := h.run(pid, "idea", "")
	if !strings.Contains(stream, "event: delta") || !strings.Contains(stream, "event: key") {
		t.Errorf("stream missing events: %s", stream)
	}
	h.run(pid, "visual_prompts", "")
	h.run(pid, "visual_images", "all")
	h.run(pid, "storyboard", "")
	h.run(pid, "shot_prompts", "all")
	h.run(pid, "keyframes", "all")

	p, _ := h.ps.Get(pid)
	if p.Name != "Demo 2" || p.AspectRatio != "16:9" || len(p.ReferenceIDs) != 1 || len(p.Shots) != 1 || len(p.Shots[0].KeyframeIDs) != 1 {
		t.Fatalf("unexpected project state: %+v", p)
	}

	page := h.project(pid)
	for _, want := range []string{"a girl &amp; a &lt;clock&gt;", "A story about &lt;Lin&gt;", "woman in red", "opens &lt;box&gt;", "/assets/" + p.Shots[0].KeyframeIDs[0] + "/file"} {
		if !strings.Contains(page, want) {
			t.Errorf("project page missing %q", want)
		}
	}

	// Shot edits only touch posted fields.
	sid := p.Shots[0].ID
	h.post("/projects/"+pid+"/shots/"+sid, url.Values{"keyframe_prompt": {"edited"}})
	p, _ = h.ps.Get(pid)
	if p.Shots[0].KeyframePrompt != "edited" || p.Shots[0].Scene != "Shop" {
		t.Errorf("partial shot update wrong: %+v", p.Shots[0])
	}

	// Attach a library asset as an explicit shot reference, then promote a keyframe.
	ref := p.ReferenceIDs[0]
	h.post("/projects/"+pid+"/refs", url.Values{"target": {"shot:" + sid}, "asset_id": {ref}, "op": {"add"}})
	p, _ = h.ps.Get(pid)
	if len(p.Shots[0].ReferenceIDs) != 1 {
		t.Errorf("ref not attached")
	}

	// Library pages and picker API.
	h.get("/assets")
	h.get("/assets?kind=keyframe&project=" + pid)
	var items []map[string]any
	if err := json.Unmarshal([]byte(h.get("/api/assets?project="+pid)), &items); err != nil || len(items) < 3 {
		t.Fatalf("api assets: %v %d", err, len(items))
	}
	if items[0]["id"] == nil || items[0]["url"] == nil {
		t.Errorf("api item fields: %v", items[0])
	}
	resp, _ = h.c.Get(h.srv.URL + "/assets/" + ref + "/file")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("asset file: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp.Body.Close()

	// Deleting an asset unlinks it everywhere.
	h.post("/assets/"+ref+"/delete", url.Values{})
	p, _ = h.ps.Get(pid)
	if len(p.ReferenceIDs) != 0 || len(p.Shots[0].ReferenceIDs) != 0 {
		t.Errorf("deleted asset still referenced: %+v", p)
	}
}

func TestSettingsAndPrompts(t *testing.T) {
	h := newHarness(t)
	page := h.get("/settings")
	if !strings.Contains(page, "ANTHROPIC_API_KEY") {
		t.Fatal("settings page missing providers")
	}
	// Rename a provider and store a key: settings that named it follow.
	h.post("/settings/providers", url.Values{"orig": {"anthropic"}, "name": {"claude"}, "type": {"anthropic"}, "key": {"sk-secret"}})
	// Add a local provider with a header.
	h.post("/settings/providers", url.Values{
		"name": {"local"}, "type": {"openai_chat"}, "base_url": {"http://localhost:11434/v1"}, "headers": {"X-Test: 1"},
	})
	c := h.s.cfg.Get()
	if c.Provider("anthropic") != nil || c.Provider("claude").APIKey != "sk-secret" || c.Stages["idea"].Provider != "claude" {
		t.Fatalf("rename not applied: %+v %+v", c.Providers, c.Stages)
	}
	if lp := c.Provider("local"); lp == nil || lp.Headers["X-Test"] != "1" {
		t.Fatalf("provider not added: %+v", c.Providers)
	}
	// An empty key field keeps the stored key; the box clears it.
	h.post("/settings/providers", url.Values{"orig": {"claude"}, "name": {"claude"}, "type": {"anthropic"}})
	if h.s.cfg.Get().Provider("claude").APIKey != "sk-secret" {
		t.Error("stored key lost on save")
	}
	page = h.get("/settings")
	if strings.Contains(page, "sk-secret") {
		t.Error("API key rendered in page")
	}
	if !strings.Contains(page, "•••• stored") || !strings.Contains(page, "localhost:11434") || !strings.Contains(page, "key stored") {
		t.Error("providers tab incomplete")
	}
	h.post("/settings/providers", url.Values{"orig": {"claude"}, "name": {"claude"}, "type": {"anthropic"}, "key_clear": {"1"}})
	if h.s.cfg.Get().Provider("claude").APIKey != "" {
		t.Error("stored key not cleared")
	}
	// Invalid input is rejected and reported.
	if u := h.postErr("/settings/providers", url.Values{"name": {"local"}, "type": {"openai_chat"}}); !strings.Contains(u.Query().Get("err"), "duplicate") {
		t.Errorf("duplicate name accepted: %s", u)
	}

	// Step defaults and image settings save separately.
	form := url.Values{"st_idea_temperature": {"0.7"}, "st_idea_max_tokens": {"900"}, "st_idea_send_images": {"1"}}
	for _, st := range config.LLMStages {
		form.Set("st_"+st+"_provider", "local")
		form.Set("st_"+st+"_model", "m-"+st)
	}
	h.post("/settings/stages", form)
	h.post("/settings/image", url.Values{
		"img_provider": {"fal"}, "img_model": {"fal-ai/x"}, "img_ref_model": {"fal-ai/y"}, "img_aspect": {"16:9"},
		"img_extra": {`{"num_inference_steps": 20}`},
	})
	c = h.s.cfg.Get()
	if sm := c.Stages["idea"]; sm.Provider != "local" || sm.Model != "m-idea" || *sm.Temperature != 0.7 || sm.MaxTokens != 900 || !sm.SendImages {
		t.Errorf("stage not saved: %+v", sm)
	}
	if c.Image.Model != "fal-ai/x" || c.Image.AspectRatio != "16:9" || c.Image.Extra["num_inference_steps"] != float64(20) {
		t.Errorf("image settings not saved: %+v", c.Image)
	}
	if u := h.postErr("/settings/image", url.Values{"img_provider": {"fal"}, "img_extra": {"[1]"}}); u.Query().Get("err") == "" {
		t.Error("bad extra JSON accepted")
	}
	for _, tab := range []string{"llm", "image"} {
		if page := h.get("/settings?tab=" + tab); !strings.Contains(page, "data-pick-model") {
			t.Errorf("tab %s has no model picker", tab)
		}
	}

	// Deleting a provider clears the settings that used it.
	h.post("/settings/providers/delete", url.Values{"name": {"local"}})
	c = h.s.cfg.Get()
	if c.Provider("local") != nil || c.Stages["idea"].Provider != "" || c.Stages["idea"].Model != "m-idea" {
		t.Errorf("delete: %+v %+v", c.Providers, c.Stages["idea"])
	}

	h.get("/prompts")
	h.post("/prompts/idea.user", url.Values{"source": {"custom {{.P.Idea}}"}})
	if !strings.Contains(h.get("/prompts"), "customised") {
		t.Error("prompt override not shown")
	}
	h.post("/prompts/idea.user/reset", url.Values{})
	if strings.Contains(h.get("/prompts"), "customised") {
		t.Error("prompt override not reset")
	}
}

func TestCrossOriginRejected(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.srv.URL+"/projects", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: got %d", resp.StatusCode)
	}
	if list, _ := h.ps.List(); len(list) != 0 {
		t.Error("project created by cross-origin request")
	}
}

func TestBackRedirectValidation(t *testing.T) {
	for in, want := range map[string]string{
		"/projects/x#s1":       "/projects/x#s1",
		"//evil.example/":      "/def",
		"https://evil.example": "/def",
		"/\\evil.example":      "/def",
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader("back="+url.QueryEscape(in)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := back(r, "/def"); got != want {
			t.Errorf("back(%q) = %q, want %q", in, got, want)
		}
	}
	if got := withQuery("/p/x#s2", "err", "a b"); got != "/p/x?err=a+b#s2" {
		t.Errorf("withQuery: %s", got)
	}
}

func TestProjectSteps(t *testing.T) {
	p := &project.Project{Idea: "x"}
	got := projectSteps(p)
	if len(got) != 7 || got[0].State != "done" || got[1].State != "" || got[5].Info != "" {
		t.Fatalf("empty project: %+v", got)
	}
	p.Story = "s"
	p.Visuals = []project.VisualPrompt{{ID: "v1", AssetIDs: []string{"a"}}, {ID: "v2"}}
	p.Shots = []project.Shot{{ID: "s1", KeyframePrompt: "k", KeyframeIDs: []string{"a"}, ClipIDs: []string{"c"}}, {ID: "s2", KeyframePrompt: "k"}}
	got = projectSteps(p)
	want := []struct{ state, info string }{
		{"done", ""}, {"done", ""}, {"partial", "1/2"}, {"done", "2"}, {"done", "2/2"}, {"partial", "1/2"}, {"partial", "1/2"},
	}
	for i, w := range want {
		if got[i].State != w.state || got[i].Info != w.info {
			t.Errorf("step %d: got %q %q, want %q %q", i+1, got[i].State, got[i].Info, w.state, w.info)
		}
	}
	if initial(" 雨夜") != "雨" || initial("abc") != "A" || initial("") != "·" {
		t.Error("initial")
	}
}

func TestRequestLogPages(t *testing.T) {
	h := newHarness(t)
	if page := h.get("/logs"); !strings.Contains(page, "Nothing recorded yet.") {
		t.Fatal("empty log page")
	}
	u := h.post("/projects", url.Values{"name": {"Logged"}, "idea": {"a quiet idea"}})
	pid := projectID(u)
	h.run(pid, "idea", "")

	page := h.get("/logs?project=" + pid)
	m := regexp.MustCompile(`href="(/logs/\d{4}-\d{2}-\d{2}/[0-9._a-z-]+)"`).FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "Idea expansion") || !strings.Contains(page, "Logged") {
		t.Fatalf("log list: %s", page)
	}
	if page := h.get("/logs?kind=image"); strings.Contains(page, m[1]) {
		t.Error("kind filter not applied")
	}
	detail := h.get(m[1])
	for _, want := range []string{"System prompt", "a quiet idea", "senior screenwriter", "claude-opus-5-5"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
	if raw := h.get(m[1] + "?raw=1"); !strings.Contains(raw, `"kind": "llm"`) {
		t.Errorf("raw: %s", raw)
	}
	resp, _ := h.c.Get(h.srv.URL + "/logs/2026-01-01/nope")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown entry: %d", resp.StatusCode)
	}
	h.post("/logs/delete", url.Values{"all": {"1"}})
	if page := h.get("/logs"); !strings.Contains(page, "Nothing recorded yet.") {
		t.Error("log not cleared")
	}
}

func TestModelLists(t *testing.T) {
	h := newHarness(t)
	var asked []string
	h.s.fetchModels = func(_ context.Context, p *config.Provider) ([]catalog.Model, error) {
		asked = append(asked, p.Name)
		if p.Name == "gemini" {
			return nil, errors.New("HTTP 401: bad key")
		}
		return []catalog.Model{
			{ID: "fal-ai/flux/dev", Name: "FLUX <dev>", Category: "text-to-image"},
			{ID: "minimax/h3-max/reference-to-video", Category: "image-to-video"},
		}, nil
	}
	getJSON := func(path string) (int, string) {
		resp, err := h.c.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Nothing cached yet.
	if code, body := getJSON("/api/models?provider=fal"); code != 200 || !strings.Contains(body, `"models":[]`) {
		t.Fatalf("empty list: %d %s", code, body)
	}
	if code, _ := getJSON("/api/models?provider=nope"); code != http.StatusNotFound {
		t.Errorf("unknown provider: %d", code)
	}

	// Fetch from the settings page: redirects with a message.
	u := h.post("/settings/providers/models", url.Values{"name": {"fal"}})
	if msg := u.Query().Get("msg"); msg != "Fetched 2 models from fal" {
		t.Errorf("message %q (%s)", msg, u)
	}
	if page := h.get("/settings"); !strings.Contains(page, "2 models") {
		t.Error("provider card does not show the model count")
	}
	_, body := getJSON("/api/models?provider=fal")
	for _, want := range []string{`"id":"fal-ai/flux/dev"`, `"category":"image-to-video"`, `"fetched_at":"20`} {
		if !strings.Contains(body, want) {
			t.Errorf("api models missing %s: %s", want, body)
		}
	}

	// Fetch from the picker (JSON), including a provider error.
	post := func(name string) (int, string) {
		req, _ := http.NewRequest("POST", h.srv.URL+"/settings/providers/models", strings.NewReader(url.Values{"name": {name}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := h.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post("fal"); code != 200 || !strings.Contains(body, `"count":2`) {
		t.Errorf("json fetch: %d %s", code, body)
	}
	if code, body := post("gemini"); code != http.StatusBadGateway || !strings.Contains(body, "bad key") {
		t.Errorf("json fetch error: %d %s", code, body)
	}
	if u := h.postErr("/settings/providers/models", url.Values{"name": {"gemini"}}); !strings.Contains(u.Query().Get("err"), "bad key") {
		t.Errorf("fetch error not reported: %s", u)
	}

	// The cached list follows a rename and goes away with the provider.
	h.post("/settings/providers", url.Values{"orig": {"fal"}, "name": {"fal2"}, "type": {"fal"}})
	if _, body := getJSON("/api/models?provider=fal2"); !strings.Contains(body, "fal-ai/flux/dev") {
		t.Error("model list did not follow the rename")
	}
	h.post("/settings/providers/delete", url.Values{"name": {"fal2"}})
	if h.s.models.Get("fal2") != nil {
		t.Error("model list not deleted with the provider")
	}
	if len(asked) != 4 {
		t.Errorf("fetches: %v", asked)
	}
}

// mp4File is the start of an MP4 file, enough to be sniffed as video.
var mp4File = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom........")

func TestVideoClips(t *testing.T) {
	h := newHarness(t)
	h.s.pipe.FetchSchema = func(context.Context, *config.Provider, string) (*imagegen.InputSchema, error) {
		return &imagegen.InputSchema{Model: config.DefaultVideoModel, Params: []imagegen.Param{
			{Name: "prompt", Type: "string"}, {Name: "first_frame_image_url", Type: "string"}, {Name: "last_frame_image_url", Type: "string"},
			{Name: "reference_video_urls", Type: "array"}, {Name: "duration", Type: "string", Enum: []string{"5", "10"}}, {Name: "resolution", Type: "string"},
		}}, nil
	}
	h.s.fetchSchema = h.s.pipe.FetchSchema
	// Use the real fal payload logic to explain the mapping.
	real, _ := imagegen.New(imagegen.Options{Type: imagegen.TypeFal})
	h.s.pipe.NewImage = func(*config.Provider) (imagegen.Generator, error) {
		return explainingFake{real.(imagegen.Explainer)}, nil
	}
	u := h.post("/projects", url.Values{"name": {"Clip"}, "idea": {"x"}})
	pid := projectID(u)
	h.run(pid, "idea", "")
	h.run(pid, "storyboard", "")
	h.run(pid, "shot_prompts", "all")
	h.run(pid, "keyframes", "all")
	p, _ := h.ps.Get(pid)
	sid := p.Shots[0].ID

	// Each list only takes its own media type.
	if u := h.upload(pid, "vref:"+sid, "ref.png", pngFile(t)); !strings.Contains(u.Query().Get("err"), "video files") {
		t.Errorf("image accepted as reference video: %s", u)
	}
	if u := h.upload(pid, "vref:"+sid, "motion.mp4", mp4File); u.Query().Get("err") != "" {
		t.Fatalf("video upload: %s", u)
	}
	// Audio is recognised by file name where sniffing cannot tell (.m4a looks like MP4).
	if u := h.upload(pid, "aref:"+sid, "voice.m4a", mp4File); u.Query().Get("err") != "" {
		t.Fatalf("audio upload: %s", u)
	}
	if u := h.upload(pid, "endframe:"+sid, "end.png", pngFile(t)); u.Query().Get("err") != "" {
		t.Fatalf("end frame upload: %s", u)
	}
	p, _ = h.ps.Get(pid)
	sh := p.Shots[0]
	if len(sh.VideoRefIDs) != 1 || len(sh.AudioRefIDs) != 1 || len(sh.EndFrameIDs) != 1 {
		t.Fatalf("lists: %+v", sh)
	}
	if u := h.postErr("/projects/"+pid+"/refs", url.Values{"target": {"aref:" + sid}, "asset_id": {sh.VideoRefIDs[0]}, "op": {"add"}}); !strings.Contains(u.Query().Get("err"), "audio files") {
		t.Errorf("video accepted as reference audio: %s", u)
	}

	stream := h.run(pid, "clips", sid)
	for _, want := range []string{"first frame → first_frame_image_url", "last frame → last_frame_image_url", "reference audio: not supported by this model", "duration → duration = 5"} {
		if !strings.Contains(stream, want) {
			t.Errorf("stream missing %q:\n%s", want, stream)
		}
	}
	p, _ = h.ps.Get(pid)
	if len(p.Shots[0].ClipIDs) != 1 {
		t.Fatalf("no clip: %+v", p.Shots[0])
	}
	page := h.project(pid)
	for _, want := range []string{`id="s7"`, "Video clips", `<video src="/assets/` + p.Shots[0].Clip() + `/file"`, "<audio", "Last frame", "7</span>"} {
		if !strings.Contains(page, want) {
			t.Errorf("project page missing %q", want)
		}
	}
	// The clip is in the library as video, and the picker can filter by media.
	if lib := h.get("/assets?media=video"); !strings.Contains(lib, `id="a-`+p.Shots[0].Clip()+`"`) || strings.Contains(lib, `id="a-`+sh.EndFrameIDs[0]+`"`) {
		t.Error("media filter on the asset library")
	}
	resp, _ := h.c.Get(h.srv.URL + "/api/assets?media=audio")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), sh.AudioRefIDs[0]) || strings.Contains(string(body), p.Shots[0].Clip()) {
		t.Errorf("api media filter: %s", body)
	}

	// Settings: the video tab shows how the cached schema maps, and saves.
	tab := h.get("/settings?tab=video")
	for _, want := range []string{"first_frame_image_url", "reference_video_urls", "not supported", "resolution", "minimax/h3-max/reference-to-video"} {
		if !strings.Contains(tab, want) {
			t.Errorf("video tab missing %q", want)
		}
	}
	h.post("/settings/video", url.Values{
		"vid_provider": {"fal"}, "vid_model": {"acme/i2v"}, "vid_extra": {`{"resolution":"1080p"}`}, "vid_mapping": {`{"last_frame":"end_image_url"}`},
	})
	vc := h.s.cfg.Get().Video
	if vc.Model != "acme/i2v" || vc.Extra["resolution"] != "1080p" || vc.Mapping["last_frame"] != "end_image_url" {
		t.Errorf("video settings: %+v", vc)
	}
	if u := h.postErr("/settings/video", url.Values{"vid_provider": {"fal"}, "vid_mapping": {`{"bogus":"x"}`}}); !strings.Contains(u.Query().Get("err"), "unknown input role") {
		t.Errorf("bad mapping accepted: %s", u)
	}
	h.post("/settings/video/schema", url.Values{})
	if h.s.models.Schema("fal", "acme/i2v") == nil {
		t.Error("schema not fetched for the new model")
	}

	// An unfinished job is shown and can be discarded.
	key := project.RunKey(project.StageClips, sid)
	h.ps.Update(pid, func(p *project.Project) error {
		p.Jobs = map[string]*project.Job{key: {Provider: "fal", Model: "acme/i2v", RequestID: "req-9", SubmittedAt: time.Now()}}
		return nil
	})
	if page := h.project(pid); !strings.Contains(page, "An earlier request is unfinished") || !strings.Contains(page, "req-9") {
		t.Error("unfinished job not shown")
	}
	h.post("/projects/"+pid+"/jobs/discard", url.Values{"key": {key}})
	if p, _ := h.ps.Get(pid); len(p.Jobs) != 0 {
		t.Errorf("job not discarded: %v", p.Jobs)
	}
}

// explainingFake generates like fakeImage but explains like the real fal
// generator.
type explainingFake struct{ imagegen.Explainer }

func (explainingFake) Generate(ctx context.Context, req *imagegen.Request) (*imagegen.Result, error) {
	return fakeImage{}.Generate(ctx, req)
}

// A project opens at the step the work has reached, each step is its own
// page, and posts go back to the page they came from.
func TestStepPages(t *testing.T) {
	h := newHarness(t)
	u := h.post("/projects", url.Values{"name": {"Demo"}, "idea": {"an idea"}})
	pid := projectID(u)
	if u.Path != "/projects/"+pid+"/step/1" {
		t.Fatalf("new project opened at %s", u.Path)
	}
	h.run(pid, "idea", "")
	h.run(pid, "storyboard", "")
	resp, err := h.c.Get(h.srv.URL + "/projects/" + pid)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Request.URL.Path; got != "/projects/"+pid+"/step/4" {
		t.Errorf("project with a storyboard opened at %s", got)
	}
	page := h.get("/projects/" + pid + "/step/4")
	if !strings.Contains(page, `id="s4"`) || strings.Contains(page, `id="s2"`) || !strings.Contains(page, `aria-current="step"`) {
		t.Error("step 4 page should show step 4 only")
	}
	for _, bad := range []string{"0", "8", "x"} {
		resp, _ := h.c.Get(h.srv.URL + "/projects/" + pid + "/step/" + bad)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("step %s: status %d", bad, resp.StatusCode)
		}
	}
	// Without a "back" a post answers with the step it belongs to.
	if u := h.post("/projects/"+pid+"/story", url.Values{"story": {"s"}}); u.Path != "/projects/"+pid+"/step/2" {
		t.Errorf("saving the story went to %s", u.Path)
	}
	back := "/projects/" + pid + "/step/6"
	if u := h.post("/projects/"+pid+"/story", url.Values{"story": {"s"}, "back": {back}}); u.Path != back {
		t.Errorf("back ignored: %s", u.Path)
	}
}

// slowLLM blocks mid-stream until released, so a test can act during a run.
type slowLLM struct{ release chan struct{} }

func (c slowLLM) Chat(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	<-c.release
	return &llm.Response{Text: "late story"}, nil
}

func (c slowLLM) Stream(ctx context.Context, req *llm.Request) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		if !yield(llm.Chunk{Delta: "early "}, nil) {
			return
		}
		select {
		case <-c.release:
		case <-ctx.Done():
			yield(llm.Chunk{}, ctx.Err())
			return
		}
		yield(llm.Chunk{Delta: "late story"}, nil)
	}
}

// A run outlives the request that started it, and another request can
// follow it from its first event.
func TestFollowRun(t *testing.T) {
	h := newHarness(t)
	slow := slowLLM{release: make(chan struct{})}
	h.s.pipe.NewLLM = func(*config.Provider) (llm.Client, error) { return slow, nil }
	pid := projectID(h.post("/projects", url.Values{"name": {"Demo"}, "idea": {"an idea"}}))
	events := "/projects/" + pid + "/events?key=idea"

	if resp, _ := h.c.Get(h.srv.URL + events); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("events without a run: %d", resp.StatusCode)
	}

	// Start the run and drop the connection once output arrives.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", h.srv.URL+"/projects/"+pid+"/run/idea", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "early") {
		n, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatalf("stream ended early: %v (%s)", err, got)
		}
		got += string(buf[:n])
	}
	cancel()
	resp.Body.Close()

	if page := h.get("/projects/" + pid + "/step/2"); !strings.Contains(page, `data-running="idea"`) || !strings.Contains(page, `data-key="idea"`) {
		t.Error("the page should mark the running step")
	}

	// A second run request for the same step follows the first one.
	type result struct{ body string }
	follow := func(method, path string) chan result {
		ch := make(chan result, 1)
		go func() {
			r, _ := http.NewRequest(method, h.srv.URL+path, nil)
			resp, err := h.c.Do(r)
			if err != nil {
				ch <- result{err.Error()}
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			ch <- result{string(b)}
		}()
		return ch
	}
	a, b := follow("GET", events), follow("POST", "/projects/"+pid+"/run/idea")
	close(slow.release)
	for _, ch := range []chan result{a, b} {
		body := (<-ch).body
		for _, want := range []string{`"idea"`, "early", "late story", "event: done"} {
			if !strings.Contains(body, want) {
				t.Errorf("followed stream missing %q: %s", want, body)
			}
		}
	}
	if p, _ := h.ps.Get(pid); !strings.Contains(p.Story, "late story") || p.Runs["idea"].Status != "done" {
		t.Errorf("run did not finish: %+v", p.Runs["idea"])
	}
	if resp, _ := h.c.Get(h.srv.URL + events); resp.StatusCode != http.StatusNotFound {
		t.Errorf("events after the run: %d", resp.StatusCode)
	}
}
