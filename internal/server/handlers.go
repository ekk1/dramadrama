package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/pipeline"
	"github.com/ekk1/dramadrama/internal/project"
)

const maxUpload = 64 << 20

// visualKinds are the kinds a visual item can have.
var visualKinds = []string{"character", "scene", "prop", "style"}

func projectURL(id string) string { return "/projects/" + id }

func notFound(w http.ResponseWriter, err error) bool {
	if errors.Is(err, project.ErrNotFound) || errors.Is(err, assets.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return true
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	return false
}

// update applies fn to the project from the path and redirects back.
func (s *Server) update(w http.ResponseWriter, r *http.Request, anchor string, fn func(p *project.Project) error) {
	id := r.PathValue("id")
	_, err := s.projects.Update(id, fn)
	if errors.Is(err, project.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	done(w, r, projectURL(id)+anchor, err)
}

// ---------------------------------------------------------------------------
// Projects

type projectSummary struct {
	*project.Project
	Thumb     string
	Keyframes int
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.projects.List()
	if notFound(w, err) {
		return
	}
	var out []projectSummary
	for _, p := range list {
		ps := projectSummary{Project: p}
		for _, sh := range p.Shots {
			ps.Keyframes += len(sh.KeyframeIDs)
			if ps.Thumb == "" && len(sh.KeyframeIDs) > 0 {
				ps.Thumb = sh.KeyframeIDs[len(sh.KeyframeIDs)-1]
			}
		}
		for _, v := range p.Visuals {
			if ps.Thumb == "" && len(v.AssetIDs) > 0 {
				ps.Thumb = v.AssetIDs[len(v.AssetIDs)-1]
			}
		}
		if ps.Thumb == "" && len(p.ReferenceIDs) > 0 {
			ps.Thumb = p.ReferenceIDs[0]
		}
		out = append(out, ps)
	}
	s.render(w, r, "projects", view{Title: "Projects", Nav: "projects", D: out})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Create(r.FormValue("name"), strings.TrimSpace(r.FormValue("idea")))
	if err != nil {
		done(w, r, "/projects", err)
		return
	}
	http.Redirect(w, r, projectURL(p.ID), http.StatusSeeOther)
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Get(r.PathValue("id"))
	if notFound(w, err) {
		return
	}
	s.render(w, r, "project", view{Title: p.Name, Nav: "projects", D: p})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	err := s.projects.Delete(r.PathValue("id"))
	if err != nil {
		done(w, r, "/projects", err)
		return
	}
	http.Redirect(w, r, withQuery("/projects", "msg", "Project deleted. Its assets remain in the library."), http.StatusSeeOther)
}

func (s *Server) handleSaveInput(w http.ResponseWriter, r *http.Request) {
	s.update(w, r, "#s1", func(p *project.Project) error {
		if n := strings.TrimSpace(r.FormValue("name")); n != "" {
			p.Name = n
		}
		p.Idea = strings.TrimSpace(r.FormValue("idea"))
		p.Requirements = strings.TrimSpace(r.FormValue("requirements"))
		if a := r.FormValue("aspect_ratio"); a != "" {
			p.AspectRatio = a
		}
		return nil
	})
}

func (s *Server) handleSaveStory(w http.ResponseWriter, r *http.Request) {
	s.update(w, r, "#s2", func(p *project.Project) error {
		p.Story = strings.TrimSpace(r.FormValue("story"))
		return nil
	})
}

func (s *Server) handleSaveStyle(w http.ResponseWriter, r *http.Request) {
	s.update(w, r, "#s3", func(p *project.Project) error {
		p.Style = strings.TrimSpace(r.FormValue("style"))
		return nil
	})
}

// ---------------------------------------------------------------------------
// Visuals

func (s *Server) handleAddVisual(w http.ResponseWriter, r *http.Request) {
	id := assets.NewID("v")
	s.update(w, r, "#v-"+id, func(p *project.Project) error {
		kind := r.FormValue("kind")
		if kind == "" {
			kind = "character"
		}
		p.Visuals = append(p.Visuals, project.VisualPrompt{ID: id, Kind: kind, Name: "New " + kind})
		return nil
	})
}

func (s *Server) handleSaveVisual(w http.ResponseWriter, r *http.Request) {
	vid := r.PathValue("vid")
	s.update(w, r, "#v-"+vid, func(p *project.Project) error {
		v := p.Visual(vid)
		if v == nil {
			return errors.New("visual item not found")
		}
		if k := r.FormValue("kind"); slices.Contains(visualKinds, k) {
			v.Kind = k
		}
		v.Name = strings.TrimSpace(r.FormValue("name"))
		v.Description = strings.TrimSpace(r.FormValue("description"))
		v.Prompt = strings.TrimSpace(r.FormValue("prompt"))
		return nil
	})
}

func (s *Server) handleDeleteVisual(w http.ResponseWriter, r *http.Request) {
	vid := r.PathValue("vid")
	s.update(w, r, "#s3", func(p *project.Project) error {
		p.Visuals = slices.DeleteFunc(p.Visuals, func(v project.VisualPrompt) bool { return v.ID == vid })
		return nil
	})
}

// ---------------------------------------------------------------------------
// Shots

func (s *Server) handleAddShot(w http.ResponseWriter, r *http.Request) {
	id := assets.NewID("s")
	s.update(w, r, "#shot-"+id, func(p *project.Project) error {
		sh := project.Shot{ID: id, Duration: 4}
		at := len(p.Shots)
		if after := r.FormValue("after"); after != "" {
			if i := p.ShotIndex(after); i >= 0 {
				at = i + 1
			}
		}
		p.Shots = slices.Insert(p.Shots, at, sh)
		p.Renumber()
		return nil
	})
}

// handleSaveShot updates only the fields present in the form, so the
// overview (stage 4) and prompt (stage 5) editors can post separately.
func (s *Server) handleSaveShot(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	r.ParseForm()
	anchor := "#shot-" + sid
	if a := r.FormValue("anchor"); a != "" {
		anchor = "#" + a
	}
	s.update(w, r, anchor, func(p *project.Project) error {
		sh := p.Shot(sid)
		if sh == nil {
			return errors.New("shot not found")
		}
		set := func(key string, dst *string) {
			if _, ok := r.PostForm[key]; ok {
				*dst = strings.TrimSpace(r.PostForm.Get(key))
			}
		}
		set("scene", &sh.Scene)
		set("action", &sh.Action)
		set("dialogue", &sh.Dialogue)
		set("camera", &sh.Camera)
		set("storyboard_prompt", &sh.StoryboardPrompt)
		set("keyframe_prompt", &sh.KeyframePrompt)
		set("motion_prompt", &sh.MotionPrompt)
		if _, ok := r.PostForm["characters"]; ok {
			sh.Characters = nil
			for _, c := range strings.FieldsFunc(r.PostForm.Get("characters"), func(r rune) bool { return r == ',' || r == '，' || r == '、' }) {
				if c = strings.TrimSpace(c); c != "" {
					sh.Characters = append(sh.Characters, c)
				}
			}
		}
		if v, ok := r.PostForm["duration"]; ok {
			d, err := strconv.ParseFloat(strings.TrimSpace(v[0]), 64)
			if err != nil && strings.TrimSpace(v[0]) != "" {
				return fmt.Errorf("duration: %v", err)
			}
			sh.Duration = d
		}
		return nil
	})
}

func (s *Server) handleDeleteShot(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	s.update(w, r, "#s4", func(p *project.Project) error {
		p.Shots = slices.DeleteFunc(p.Shots, func(sh project.Shot) bool { return sh.ID == sid })
		p.Renumber()
		return nil
	})
}

func (s *Server) handleMoveShot(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	s.update(w, r, "#shot-"+sid, func(p *project.Project) error {
		i := p.ShotIndex(sid)
		if i < 0 {
			return errors.New("shot not found")
		}
		j := i - 1
		if r.FormValue("dir") == "down" {
			j = i + 1
		}
		if j >= 0 && j < len(p.Shots) {
			p.Shots[i], p.Shots[j] = p.Shots[j], p.Shots[i]
			p.Renumber()
		}
		return nil
	})
}

// handleApplyRaw applies hand-edited JSON for visuals or the storyboard.
func (s *Server) handleApplyRaw(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw := r.FormValue("raw")
	var err error
	anchor := "#s3"
	switch r.PathValue("kind") {
	case "visuals":
		err = s.pipe.ApplyVisualsJSON(id, raw)
	case "storyboard":
		err, anchor = s.pipe.ApplyStoryboardJSON(id, raw), "#s4"
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, projectURL(id)+anchor, err)
}

// ---------------------------------------------------------------------------
// Asset references

// refList resolves a target spec to the slice it designates:
//
//	project            style references of the project
//	visual:<id>        reference images of a visual item
//	visual-out:<id>    generated images of a visual item
//	shot:<id>          explicit reference images of a shot
//	keyframe:<id>      keyframes of a shot
func refList(p *project.Project, target string) (*[]string, error) {
	kind, id, _ := strings.Cut(target, ":")
	switch kind {
	case "project":
		return &p.ReferenceIDs, nil
	case "visual", "visual-out":
		v := p.Visual(id)
		if v == nil {
			return nil, errors.New("visual item not found")
		}
		if kind == "visual" {
			return &v.ReferenceIDs, nil
		}
		return &v.AssetIDs, nil
	case "shot", "keyframe":
		sh := p.Shot(id)
		if sh == nil {
			return nil, errors.New("shot not found")
		}
		if kind == "shot" {
			return &sh.ReferenceIDs, nil
		}
		return &sh.KeyframeIDs, nil
	}
	return nil, fmt.Errorf("unknown target %q", target)
}

// handleRefs adds, removes or promotes (op=primary: move to the end, which
// marks it as the one used downstream) an asset in a reference list.
func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	aid := r.FormValue("asset_id")
	if _, err := s.assets.Get(aid); err != nil {
		done(w, r, projectURL(r.PathValue("id")), err)
		return
	}
	s.update(w, r, "", func(p *project.Project) error {
		list, err := refList(p, r.FormValue("target"))
		if err != nil {
			return err
		}
		without := slices.DeleteFunc(slices.Clone(*list), func(x string) bool { return x == aid })
		switch r.FormValue("op") {
		case "add", "primary":
			*list = append(without, aid)
		case "remove":
			*list = without
		default:
			return errors.New("unknown op")
		}
		return nil
	})
}

// saveUploads stores multipart files as assets.
func (s *Server) saveUploads(w http.ResponseWriter, r *http.Request, meta func() assets.Asset) ([]*assets.Asset, error) {
	// Cap the whole request body, and keep at most 8 MB of it in memory
	// (larger files are spooled to temporary files by the parser). This
	// must happen before anything reads a form value.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errors.New("upload too large (limit 64 MB)")
		}
		return nil, err
	}
	defer r.MultipartForm.RemoveAll()
	m := meta()
	var out []*assets.Asset
	for _, fh := range r.MultipartForm.File["files"] {
		a, err := s.saveUpload(fh, m)
		if err != nil {
			return out, fmt.Errorf("%s: %w", fh.Filename, err)
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, errors.New("no files uploaded")
	}
	return out, nil
}

func (s *Server) saveUpload(fh *multipart.FileHeader, meta assets.Asset) (*assets.Asset, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload))
	if err != nil {
		return nil, err
	}
	mt := http.DetectContentType(data)
	if !strings.HasPrefix(mt, "image/") && !strings.HasPrefix(mt, "video/") {
		return nil, fmt.Errorf("unsupported file type %s", mt)
	}
	meta.MIME = mt
	if meta.Name == "" {
		meta.Name = fh.Filename
	}
	return s.assets.Put(meta, data)
}

func (s *Server) handleProjectUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.projects.Get(id); notFound(w, err) {
		return
	}
	uploaded, err := s.saveUploads(w, r, func() assets.Asset {
		return assets.Asset{Kind: assets.KindReference, ProjectID: id, Stage: project.StageInput, Tags: []string{"upload"}}
	})
	if len(uploaded) > 0 {
		target := r.FormValue("target")
		_, uerr := s.projects.Update(id, func(p *project.Project) error {
			list, err := refList(p, target)
			if err != nil {
				return err
			}
			for _, a := range uploaded {
				*list = append(*list, a.ID)
			}
			return nil
		})
		if err == nil {
			err = uerr
		}
	}
	done(w, r, projectURL(id), err)
}

// ---------------------------------------------------------------------------
// Running steps (Server-Sent Events)

// handleRun executes one step and streams progress as SSE. The step runs
// detached from the request: closing the page does not stop it (use
// /cancel), and the result is saved either way.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.projects.Get(id); notFound(w, err) {
		return
	}
	req := pipeline.RunRequest{
		ProjectID:   id,
		Stage:       r.PathValue("stage"),
		Target:      r.FormValue("target"),
		Provider:    strings.TrimSpace(r.FormValue("provider")),
		Model:       strings.TrimSpace(r.FormValue("model")),
		OnlyMissing: r.FormValue("only_missing") != "",
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	gone := false
	send := func(event, data string) {
		mu.Lock()
		defer mu.Unlock()
		if gone {
			return
		}
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			gone = true
			return
		}
		flusher.Flush()
	}
	send("key", project.RunKey(req.Stage, req.Target))

	stop := make(chan struct{})
	go func() { // keep-alive while waiting on slow providers
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				mu.Lock()
				gone = true
				mu.Unlock()
				return
			case <-t.C:
				send("ping", "")
			}
		}
	}()
	tr := translator(langOf(r))
	err := s.pipe.Run(context.WithoutCancel(r.Context()), req, func(e pipeline.Event) {
		if e.Type == "status" {
			e.Text = tr(e.Text)
		}
		send(e.Type, e.Text)
	})
	close(stop)
	if err != nil {
		log.Printf("run %s %s %s: %v", id, req.Stage, req.Target, err)
		send("error", tr(err.Error()))
		return
	}
	send("done", "ok")
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.pipe.Cancel(id, r.FormValue("key")) {
		done(w, r, projectURL(id), errors.New("that step is not running"))
		return
	}
	done(w, r, projectURL(id), nil)
}

// ---------------------------------------------------------------------------
// Asset library

type assetsView struct {
	Assets   []*assets.Asset
	Projects map[string]string
	Tags     []string
	F        assets.Filter
}

func (s *Server) filter(r *http.Request) assets.Filter {
	q := r.URL.Query()
	return assets.Filter{
		ProjectID: q.Get("project"),
		Kind:      assets.Kind(q.Get("kind")),
		Tag:       q.Get("tag"),
		Query:     q.Get("q"),
	}
}

func (s *Server) projectNames() map[string]string {
	names := map[string]string{}
	if list, err := s.projects.List(); err == nil {
		for _, p := range list {
			names[p.ID] = p.Name
		}
	}
	return names
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	f := s.filter(r)
	s.render(w, r, "assets", view{Title: "Asset library", Nav: "assets", D: assetsView{
		Assets: s.assets.List(f), Projects: s.projectNames(), Tags: s.assets.Tags(), F: f,
	}})
}

func (s *Server) handleLibraryUpload(w http.ResponseWriter, r *http.Request) {
	_, err := s.saveUploads(w, r, func() assets.Asset {
		kind := assets.Kind(r.FormValue("kind"))
		if !slices.Contains(assets.Kinds, kind) {
			kind = assets.KindReference
		}
		return assets.Asset{Kind: kind, ProjectID: r.FormValue("project"), Tags: assets.ParseTags(r.FormValue("tags"))}
	})
	done(w, r, "/assets", err)
}

func (s *Server) handleAssetFile(w http.ResponseWriter, r *http.Request) {
	a, err := s.assets.Get(r.PathValue("aid"))
	if notFound(w, err) {
		return
	}
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, a.ID, a.Ext))
	}
	http.ServeFile(w, r, s.assets.FilePath(a))
}

func (s *Server) handleUpdateAsset(w http.ResponseWriter, r *http.Request) {
	_, err := s.assets.Update(r.PathValue("aid"), func(a *assets.Asset) {
		a.Name = strings.TrimSpace(r.FormValue("name"))
		a.Tags = assets.ParseTags(r.FormValue("tags"))
		if k := assets.Kind(r.FormValue("kind")); slices.Contains(assets.Kinds, k) {
			a.Kind = k
		}
	})
	done(w, r, "/assets", err)
}

// handleDeleteAsset removes an asset and every project reference to it.
func (s *Server) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	if err := s.assets.Delete(aid); err != nil {
		done(w, r, "/assets", err)
		return
	}
	list, _ := s.projects.List()
	for _, p := range list {
		s.projects.Update(p.ID, func(p *project.Project) error {
			p.RemoveAssetRefs(aid)
			return nil
		})
	}
	done(w, r, "/assets", nil)
}

// handleAPIAssets serves the asset picker.
func (s *Server) handleAPIAssets(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		MIME    string `json:"mime"`
		URL     string `json:"url"`
		Project string `json:"project"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
	}
	names := s.projectNames()
	out := []item{}
	for _, a := range s.assets.List(s.filter(r)) {
		out = append(out, item{
			ID: a.ID, Name: a.Name, Kind: string(a.Kind), MIME: a.MIME, URL: "/assets/" + a.ID + "/file",
			Project: names[a.ProjectID], Width: a.Width, Height: a.Height,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
