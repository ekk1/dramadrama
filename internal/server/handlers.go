package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/project"
)

// visualKinds are the kinds a visual item can have.
var visualKinds = []string{"character", "scene", "prop", "style"}

func projectURL(id string) string { return "/projects/" + id }

// stepURL is the page of step n of a project.
func stepURL(id string, n int) string { return fmt.Sprintf("/projects/%s/step/%d", id, n) }

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

// update applies fn to the project from the path and redirects back, by
// default to `where` below the project's URL, e.g. "/step/4#shot-x".
func (s *Server) update(w http.ResponseWriter, r *http.Request, where string, fn func(p *project.Project) error) {
	id := r.PathValue("id")
	_, err := s.projects.Update(id, fn)
	if errors.Is(err, project.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	done(w, r, projectURL(id)+where, err)
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

// projectView is the data of one step page of a project.
type projectView struct {
	P    *project.Project
	Step int
}

// handleProject opens a project at the step the work has reached.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Get(r.PathValue("id"))
	if notFound(w, err) {
		return
	}
	step := 1
	for _, st := range projectSteps(p) {
		if st.State != "" {
			step = st.Num
		}
	}
	target := stepURL(p.ID, step)
	if r.URL.RawQuery != "" { // keep an error message from a post
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleStep shows one step of a project.
func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	p, err := s.projects.Get(r.PathValue("id"))
	if notFound(w, err) {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 || n > len(projectSteps(p)) {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "project", view{Title: p.Name, Nav: "projects", D: projectView{P: p, Step: n}})
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
	s.update(w, r, "/step/1", func(p *project.Project) error {
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
	s.update(w, r, "/step/2", func(p *project.Project) error {
		p.Story = strings.TrimSpace(r.FormValue("story"))
		return nil
	})
}

func (s *Server) handleSaveStyle(w http.ResponseWriter, r *http.Request) {
	s.update(w, r, "/step/3", func(p *project.Project) error {
		p.Style = strings.TrimSpace(r.FormValue("style"))
		return nil
	})
}

// ---------------------------------------------------------------------------
// Visuals

func (s *Server) handleAddVisual(w http.ResponseWriter, r *http.Request) {
	id := assets.NewID("v")
	s.update(w, r, "/step/3#v-"+id, func(p *project.Project) error {
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
	s.update(w, r, "/step/3#v-"+vid, func(p *project.Project) error {
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
	s.update(w, r, "/step/3", func(p *project.Project) error {
		p.Visuals = slices.DeleteFunc(p.Visuals, func(v project.VisualPrompt) bool { return v.ID == vid })
		return nil
	})
}

// ---------------------------------------------------------------------------
// Shots

func (s *Server) handleAddShot(w http.ResponseWriter, r *http.Request) {
	id := assets.NewID("s")
	s.update(w, r, "/step/4#shot-"+id, func(p *project.Project) error {
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
	where := "/step/4#shot-" + sid
	if r.Form.Has("keyframe_prompt") { // the prompt editor of step 5
		where = "/step/5#sp-" + sid
	}
	s.update(w, r, where, func(p *project.Project) error {
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
	s.update(w, r, "/step/4", func(p *project.Project) error {
		p.Shots = slices.DeleteFunc(p.Shots, func(sh project.Shot) bool { return sh.ID == sid })
		p.Renumber()
		return nil
	})
}

func (s *Server) handleMoveShot(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	s.update(w, r, "/step/4#shot-"+sid, func(p *project.Project) error {
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
	step := 3
	switch r.PathValue("kind") {
	case "visuals":
		err = s.pipe.ApplyVisualsJSON(id, raw)
	case "storyboard":
		err, step = s.pipe.ApplyStoryboardJSON(id, raw), 4
	default:
		http.NotFound(w, r)
		return
	}
	done(w, r, stepURL(id, step), err)
}
