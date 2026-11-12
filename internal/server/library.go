package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/project"
)

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
		Media:     q.Get("media"),
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
	_, err := s.saveUploads(w, r, func() (assets.Asset, string) {
		kind := assets.Kind(r.FormValue("kind"))
		if !slices.Contains(assets.Kinds, kind) {
			kind = assets.KindReference
		}
		return assets.Asset{Kind: kind, ProjectID: r.FormValue("project"), Tags: assets.ParseTags(r.FormValue("tags"))}, ""
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
