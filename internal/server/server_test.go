package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	pipe.NewLLM = func(*config.Provider) (llm.Client, error) { return fakeLLM{}, nil }
	pipe.NewImage = func(*config.Provider) (imagegen.Generator, error) { return fakeImage{}, nil }
	models, _ := catalog.Open(filepath.Join(dir, "models"))
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

func pngFile(t *testing.T) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	return buf.Bytes()
}

func TestWorkbenchFlow(t *testing.T) {
	h := newHarness(t)
	h.get("/projects")
	u := h.post("/projects", url.Values{"name": {"Demo"}, "idea": {"time machine"}})
	pid := strings.TrimPrefix(u.Path, "/projects/")
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

	page := h.get("/projects/" + pid)
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
	if len(got) != 6 || got[0].State != "done" || got[1].State != "" || got[5].Info != "" {
		t.Fatalf("empty project: %+v", got)
	}
	p.Story = "s"
	p.Visuals = []project.VisualPrompt{{ID: "v1", AssetIDs: []string{"a"}}, {ID: "v2"}}
	p.Shots = []project.Shot{{ID: "s1", KeyframePrompt: "k", KeyframeIDs: []string{"a"}}, {ID: "s2", KeyframePrompt: "k"}}
	got = projectSteps(p)
	want := []struct{ state, info string }{
		{"done", ""}, {"done", ""}, {"partial", "1/2"}, {"done", "2"}, {"done", "2/2"}, {"partial", "1/2"},
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
	pid := strings.TrimPrefix(u.Path, "/projects/")
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
