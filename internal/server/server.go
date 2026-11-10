// Package server is the web workbench: server-rendered pages (html/template)
// with a small amount of JavaScript for streaming step output and picking
// assets from the library.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/catalog"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
	"github.com/ekk1/dramadrama/internal/pipeline"
	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
	"github.com/ekk1/dramadrama/web"
)

// Options configures the server.
type Options struct {
	Config   *config.Manager
	Projects *project.Store
	Assets   *assets.Store
	Pipeline *pipeline.Service
	Prompts  *pipeline.Prompts
	Logs     *reqlog.Store
	Models   *catalog.Store
	// DevDir, when set, serves templates and static files from this
	// directory on disk (re-parsed per request) instead of the embedded copy.
	DevDir string
}

// Server implements http.Handler.
type Server struct {
	cfg      *config.Manager
	projects *project.Store
	assets   *assets.Store
	pipe     *pipeline.Service
	prompts  *pipeline.Prompts
	logs     *reqlog.Store
	models   *catalog.Store
	// fetchModels downloads a provider's model list; replaceable in tests.
	fetchModels func(ctx context.Context, p *config.Provider) ([]catalog.Model, error)
	// fetchSchema downloads a model's input schema; replaceable in tests.
	fetchSchema func(ctx context.Context, p *config.Provider, model string) (*imagegen.InputSchema, error)
	files       fs.FS
	dev         bool

	mu    sync.Mutex
	pages map[string]*template.Template
	mux   *http.ServeMux
}

// New builds the server and parses the templates.
func New(o Options) (*Server, error) {
	s := &Server{
		cfg: o.Config, projects: o.Projects, assets: o.Assets, pipe: o.Pipeline, prompts: o.Prompts, logs: o.Logs, models: o.Models, fetchModels: catalog.Fetch, fetchSchema: catalog.FetchSchema,
		files: web.FS, pages: map[string]*template.Template{},
	}
	if o.DevDir != "" {
		s.files, s.dev = os.DirFS(o.DevDir), true
	}
	for _, page := range []string{"projects", "project", "assets", "settings", "prompts", "logs", "log"} {
		if _, err := s.page(page); err != nil {
			return nil, err
		}
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(s.files, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/projects", http.StatusSeeOther)
	})

	m.HandleFunc("GET /projects", s.handleProjects)
	m.HandleFunc("POST /projects", s.handleCreateProject)
	m.HandleFunc("GET /projects/{id}", s.handleProject)
	m.HandleFunc("POST /projects/{id}/delete", s.handleDeleteProject)
	m.HandleFunc("POST /projects/{id}/input", s.handleSaveInput)
	m.HandleFunc("POST /projects/{id}/story", s.handleSaveStory)
	m.HandleFunc("POST /projects/{id}/style", s.handleSaveStyle)
	m.HandleFunc("POST /projects/{id}/visuals", s.handleAddVisual)
	m.HandleFunc("POST /projects/{id}/visuals/{vid}", s.handleSaveVisual)
	m.HandleFunc("POST /projects/{id}/visuals/{vid}/delete", s.handleDeleteVisual)
	m.HandleFunc("POST /projects/{id}/shots", s.handleAddShot)
	m.HandleFunc("POST /projects/{id}/shots/{sid}", s.handleSaveShot)
	m.HandleFunc("POST /projects/{id}/shots/{sid}/delete", s.handleDeleteShot)
	m.HandleFunc("POST /projects/{id}/shots/{sid}/move", s.handleMoveShot)
	m.HandleFunc("POST /projects/{id}/apply/{kind}", s.handleApplyRaw)
	m.HandleFunc("POST /projects/{id}/refs", s.handleRefs)
	m.HandleFunc("POST /projects/{id}/upload", s.handleProjectUpload)
	m.HandleFunc("POST /projects/{id}/run/{stage}", s.handleRun)
	m.HandleFunc("POST /projects/{id}/cancel", s.handleCancel)
	m.HandleFunc("POST /projects/{id}/jobs/discard", s.handleDiscardJob)

	m.HandleFunc("GET /assets", s.handleAssets)
	m.HandleFunc("POST /assets/upload", s.handleLibraryUpload)
	m.HandleFunc("GET /assets/{aid}/file", s.handleAssetFile)
	m.HandleFunc("POST /assets/{aid}", s.handleUpdateAsset)
	m.HandleFunc("POST /assets/{aid}/delete", s.handleDeleteAsset)
	m.HandleFunc("GET /api/assets", s.handleAPIAssets)

	m.HandleFunc("GET /logs", s.handleLogs)
	m.HandleFunc("GET /logs/{day}/{name}", s.handleLog)
	m.HandleFunc("POST /logs/delete", s.handleDeleteLogs)

	m.HandleFunc("GET /settings", s.handleSettings)
	m.HandleFunc("POST /settings/providers", s.handleSaveProvider)
	m.HandleFunc("POST /settings/providers/delete", s.handleDeleteProvider)
	m.HandleFunc("POST /settings/providers/models", s.handleFetchModels)
	m.HandleFunc("POST /settings/stages", s.handleSaveStages)
	m.HandleFunc("POST /settings/image", s.handleSaveImage)
	m.HandleFunc("POST /settings/video", s.handleSaveVideo)
	m.HandleFunc("POST /settings/video/schema", s.handleFetchSchema)
	m.HandleFunc("GET /api/models", s.handleAPIModels)
	m.HandleFunc("GET /lang/{lang}", s.handleLang)
	m.HandleFunc("GET /prompts", s.handlePrompts)
	m.HandleFunc("POST /prompts/{name}", s.handleSavePrompt)
	m.HandleFunc("POST /prompts/{name}/reset", s.handleResetPrompt)
	s.mux = m
}

// ServeHTTP applies a same-origin check to state-changing requests (the
// workbench has no login, so this blocks other websites from driving it
// through the user's browser) and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
		http.Error(w, translator(langOf(r))("cross-origin request rejected"), http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Rendering

// view is the data passed to every page template.
type view struct {
	Title string
	Nav   string
	Err   string
	Msg   string
	Lang  string
	Path  string            // current path + query, for the language switch
	JS    map[string]string // translated strings for app.js
	D     any
}

func (s *Server) page(name string) (*template.Template, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.pages[name]; ok && !s.dev {
		return t, nil
	}
	t, err := template.New("").Funcs(s.funcs()).ParseFS(s.files,
		"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", name, err)
	}
	s.pages[name] = t
	return t, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, v view) {
	t, err := s.page(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Bind the request's language: templates call {{t "English text"}}.
	lang := langOf(r)
	tr := translator(lang)
	t, err = t.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t.Funcs(template.FuncMap{"t": tr})
	v.Lang, v.JS, v.Path = lang, jsStrings(tr), r.URL.RequestURI()
	v.Title = tr(v.Title)
	v.Err = tr(r.URL.Query().Get("err"))
	v.Msg = tr(r.URL.Query().Get("msg"))
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", v); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// jobView is an unfinished provider job, for display.
type jobView struct {
	Key, RequestID, Model, When string
}

// runView is a display-friendly run record.
type runView struct {
	Status, Error, Provider, Model, When, Key string
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"asset": func(id string) *assets.Asset {
			a, err := s.assets.Get(id)
			if err != nil {
				return nil
			}
			return a
		},
		"run": func(p *project.Project, stage, target string) *runView {
			key := project.RunKey(stage, target)
			r := p.Runs[key]
			if r == nil {
				return nil
			}
			v := &runView{Status: r.Status, Error: r.Error, Provider: r.Provider, Model: r.Model, Key: key, When: fmtTime(r.StartedAt)}
			if r.Status == "running" && !s.pipe.IsRunning(p.ID, key) {
				v.Status, v.Error = "error", "interrupted (server restarted)"
			}
			return v
		},
		"job": func(p *project.Project, stage, target string) *jobView {
			key := project.RunKey(stage, target)
			j := p.Jobs[key]
			if j == nil || s.pipe.IsRunning(p.ID, key) || s.pipe.IsRunning(p.ID, project.RunKey(stage, "all")) {
				return nil
			}
			return &jobView{Key: key, RequestID: j.RequestID, Model: j.Model, When: fmtTime(j.SubmittedAt)}
		},
		// nextKeyframe is the keyframe in use of the shot after index i.
		"nextKeyframe": func(p *project.Project, i int) string {
			if i+1 < len(p.Shots) {
				return p.Shots[i+1].Keyframe()
			}
			return ""
		},
		"videoCfg": func() config.VideoConfig { return s.cfg.Get().Video },
		"shotRefs": func(p *project.Project, sh *project.Shot) map[string]any {
			ids, auto := pipeline.ShotRefs(p, sh)
			return map[string]any{"IDs": ids, "Auto": auto}
		},
		"stageModel": func(stage string) config.StageModel { return s.cfg.Get().Stages[stage] },
		"imageCfg":   func() config.ImageConfig { return s.cfg.Get().Image },
		"providers": func(kind string) []config.Provider {
			var out []config.Provider
			for _, p := range s.cfg.Get().Providers {
				if (kind == "llm" && p.IsLLM()) || (kind == "image" && p.IsImage()) {
					out = append(out, p)
				}
			}
			return out
		},
		"last": func(ids []string) string {
			if len(ids) == 0 {
				return ""
			}
			return ids[len(ids)-1]
		},
		"steps":      projectSteps,
		"initial":    initial,
		"arClass":    func(ratio string) string { return "ar-" + strings.ReplaceAll(ratio, ":", "-") },
		"t":          func(s string) string { return s }, // replaced per request in render
		"join":       strings.Join,
		"time":       fmtTime,
		"clock":      func(t time.Time) string { return t.Local().Format("15:04:05") },
		"second":     func(t time.Time) string { return t.Local().Format("05") },
		"duration":   fmtDuration,
		"mediaTypes": func() []string { return []string{"image", "video", "audio"} },
		"mediaName": func(m string) string {
			return map[string]string{"image": "Image", "video": "Video", "audio": "Audio"}[m]
		},
		"logKind":   logKind,
		"stageName": stageName,
		"logKinds":  func() []string { return []string{reqlog.KindLLM, reqlog.KindImage, reqlog.KindVideo} },
		"size":      fmtSize,
		"dict":      dict,
		"add":       func(a, b int) int { return a + b },
		"trunc":     truncate,
		"lower":     strings.ToLower,
		"hasPref":   strings.HasPrefix,
		"json": func(v any) string {
			b, _ := json.MarshalIndent(v, "", "  ")
			return string(b)
		},
		"kinds":       func() []assets.Kind { return assets.Kinds },
		"llmTypes":    func() []string { return llm.Types },
		"imageTypes":  func() []string { return imagegen.Types },
		"llmStages":   func() []string { return config.LLMStages },
		"aspects":     func() []string { return []string{"9:16", "16:9", "3:4", "4:3", "1:1"} },
		"visualKinds": func() []string { return visualKinds },
	}
}

// stepView describes one pipeline step for the step navigation.
type stepView struct {
	Num   int
	Label string // translation key
	State string // "done", "partial" or ""
	Info  string // short counter such as "3" or "2/8"
}

// projectSteps summarises how far a project has progressed through the
// steps. A step is "done" when it has output for every item it covers
// and "partial" when only some items have output.
func projectSteps(p *project.Project) []stepView {
	state := func(n, of int) string {
		switch {
		case of > 0 && n >= of:
			return "done"
		case n > 0:
			return "partial"
		}
		return ""
	}
	flag := func(ok bool) string {
		if ok {
			return "done"
		}
		return ""
	}
	count := func(n int) string {
		if n == 0 {
			return ""
		}
		return fmt.Sprint(n)
	}
	ratio := func(n, of int) string {
		if of == 0 {
			return ""
		}
		return fmt.Sprintf("%d/%d", n, of)
	}
	var images, prompts, keyframes, clips int
	for _, v := range p.Visuals {
		if len(v.AssetIDs) > 0 {
			images++
		}
	}
	for _, sh := range p.Shots {
		if sh.KeyframePrompt != "" {
			prompts++
		}
		if len(sh.KeyframeIDs) > 0 {
			keyframes++
		}
		if len(sh.ClipIDs) > 0 {
			clips++
		}
	}
	return []stepView{
		{1, "Input", flag(strings.TrimSpace(p.Idea) != ""), ""},
		{2, "Story", flag(strings.TrimSpace(p.Story) != ""), ""},
		{3, "Visuals", state(images, len(p.Visuals)), ratio(images, len(p.Visuals))},
		{4, "Storyboard", flag(len(p.Shots) > 0), count(len(p.Shots))},
		{5, "Shot prompts", state(prompts, len(p.Shots)), ratio(prompts, len(p.Shots))},
		{6, "Keyframes", state(keyframes, len(p.Shots)), ratio(keyframes, len(p.Shots))},
		{7, "Video", state(clips, len(p.Shots)), ratio(clips, len(p.Shots))},
	}
}

// initial returns the first character of a name, for image placeholders.
func initial(name string) string {
	for _, r := range strings.TrimSpace(name) {
		return strings.ToUpper(string(r))
	}
	return "·"
}

func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

func fmtSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---------------------------------------------------------------------------
// Redirect helpers

// back returns the validated "back" form value or def.
func back(r *http.Request, def string) string {
	b := r.FormValue("back")
	if strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") && !strings.HasPrefix(b, "/\\") {
		return b
	}
	return def
}

func withQuery(target, key, val string) string {
	frag := ""
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target, frag = target[:i], target[i:]
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + key + "=" + url.QueryEscape(val) + frag
}

// done redirects back after a form post, carrying an error if any.
func done(w http.ResponseWriter, r *http.Request, def string, err error) {
	target := back(r, def)
	if err != nil {
		target = withQuery(target, "err", err.Error())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleLang stores the interface language in a cookie and goes back.
func (s *Server) handleLang(w http.ResponseWriter, r *http.Request) {
	lang := r.PathValue("lang")
	if lang != LangEN && lang != LangZH {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: langCookie, Value: lang, Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, back(r, "/projects"), http.StatusSeeOther)
}
