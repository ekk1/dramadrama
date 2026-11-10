package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ekk1/dramadrama/internal/catalog"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/llm"
)

// Settings tabs.
var settingsTabs = []string{"providers", "llm", "image"}

// providerView is a provider prepared for display. The stored key is never
// included.
type providerView struct {
	config.Provider
	Index   int
	KeySet  bool // a key is stored in the config file
	EnvSet  bool // the environment variable is set
	NeedKey bool // no key at all, and this type needs one
	Headers string
	// Cached model list.
	Models    int
	FetchedAt time.Time
}

type settingsView struct {
	Tab    string
	LLM    []providerView
	Media  []providerView
	Stages map[string]config.StageModel
	Image  config.ImageConfig
	Extra  string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	v := settingsView{Tab: r.URL.Query().Get("tab"), Stages: c.Stages, Image: c.Image}
	if !slices.Contains(settingsTabs, v.Tab) {
		v.Tab = settingsTabs[0]
	}
	for i, p := range c.Providers {
		pv := providerView{
			Provider: p, Index: i, KeySet: p.APIKey != "",
			EnvSet:  p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) != "",
			NeedKey: !p.HasKey() && p.Type != llm.TypeOpenAIChat,
		}
		pv.APIKey = "" // never render secrets
		var hs []string
		for k, val := range p.Headers {
			hs = append(hs, k+": "+val)
		}
		slices.Sort(hs)
		pv.Headers = strings.Join(hs, "\n")
		if l := s.models.Get(p.Name); l != nil {
			pv.Models, pv.FetchedAt = len(l.Models), l.FetchedAt
		}
		if p.IsLLM() {
			v.LLM = append(v.LLM, pv)
		} else {
			v.Media = append(v.Media, pv)
		}
	}
	if len(c.Image.Extra) > 0 {
		b, _ := json.MarshalIndent(c.Image.Extra, "", "  ")
		v.Extra = string(b)
	}
	s.render(w, r, "settings", view{Title: "Settings", Nav: "settings", D: v})
}

func settingsURL(tab string) string { return "/settings?tab=" + tab }

// settingsDone redirects back to a settings tab with "Saved" or the error.
func settingsDone(w http.ResponseWriter, r *http.Request, tab string, err error) {
	target := settingsURL(tab)
	if err != nil {
		target = withQuery(target, "err", err.Error())
	} else {
		target = withQuery(target, "msg", "Saved")
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renameProvider points every setting that names a provider at its new name.
func renameProvider(c *config.Config, from, to string) {
	for st, sm := range c.Stages {
		if sm.Provider == from {
			sm.Provider = to
			c.Stages[st] = sm
		}
	}
	if c.Image.Provider == from {
		c.Image.Provider = to
	}
}

// handleSaveProvider creates a provider (orig empty) or updates the one
// named orig. An empty key field keeps the stored key.
func (s *Server) handleSaveProvider(w http.ResponseWriter, r *http.Request) {
	orig, name := r.FormValue("orig"), strings.TrimSpace(r.FormValue("name"))
	err := s.cfg.Update(func(c *config.Config) error {
		var p *config.Provider
		if orig == "" {
			c.Providers = append(c.Providers, config.Provider{})
			p = &c.Providers[len(c.Providers)-1]
		} else if p = c.Provider(orig); p == nil {
			return errors.New("provider not found")
		}
		p.Name = name
		p.Type = r.FormValue("type")
		p.BaseURL = strings.TrimSpace(r.FormValue("base_url"))
		p.APIKeyEnv = strings.TrimSpace(r.FormValue("key_env"))
		if key := strings.TrimSpace(r.FormValue("key")); key != "" {
			p.APIKey = key
		} else if r.FormValue("key_clear") != "" {
			p.APIKey = ""
		}
		p.Headers = nil
		for _, line := range strings.Split(r.FormValue("headers"), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(k) != "" {
				if p.Headers == nil {
					p.Headers = map[string]string{}
				}
				p.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if orig != "" && orig != name {
			renameProvider(c, orig, name)
		}
		return nil
	})
	if err == nil && orig != "" && orig != name {
		s.models.Rename(orig, name)
	}
	settingsDone(w, r, "providers", err)
}

// handleDeleteProvider removes a provider and clears settings that used it.
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	err := s.cfg.Update(func(c *config.Config) error {
		if c.Provider(name) == nil {
			return errors.New("provider not found")
		}
		c.Providers = slices.DeleteFunc(c.Providers, func(p config.Provider) bool { return p.Name == name })
		renameProvider(c, name, "")
		return nil
	})
	if err == nil {
		s.models.Delete(name)
	}
	settingsDone(w, r, "providers", err)
}

// handleFetchModels downloads a provider's model list and caches it. The
// model picker calls it with Accept: application/json.
func (s *Server) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	wantJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	fail := func(err error) {
		if wantJSON {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": translator(langOf(r))(err.Error())})
			return
		}
		http.Redirect(w, r, withQuery(settingsURL("providers"), "err", err.Error()), http.StatusSeeOther)
	}
	p := s.cfg.Get().Provider(name)
	if p == nil {
		fail(errors.New("provider not found"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	list, err := s.fetchModels(ctx, p)
	if err == nil && len(list) == 0 {
		err = errors.New("the provider returned no models")
	}
	if err == nil {
		_, err = s.models.Save(name, list)
	}
	if err != nil {
		fail(err)
		return
	}
	if wantJSON {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"count": len(list)})
		return
	}
	msg := fmt.Sprintf("Fetched %d models from %s", len(list), name)
	http.Redirect(w, r, withQuery(settingsURL("providers"), "msg", msg), http.StatusSeeOther)
}

// handleAPIModels serves the model picker.
func (s *Server) handleAPIModels(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("provider")
	out := struct {
		Provider  string          `json:"provider"`
		FetchedAt string          `json:"fetched_at"`
		Models    []catalog.Model `json:"models"`
	}{Provider: name, Models: []catalog.Model{}}
	if s.cfg.Get().Provider(name) == nil {
		http.Error(w, "provider not found", http.StatusNotFound)
		return
	}
	if l := s.models.Get(name); l != nil {
		out.FetchedAt, out.Models = fmtTime(l.FetchedAt), l.Models
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleSaveStages stores the default provider and model of each LLM step.
func (s *Server) handleSaveStages(w http.ResponseWriter, r *http.Request) {
	err := s.cfg.Update(func(c *config.Config) error {
		for _, st := range config.LLMStages {
			sm := config.StageModel{
				Provider:   r.FormValue("st_" + st + "_provider"),
				Model:      strings.TrimSpace(r.FormValue("st_" + st + "_model")),
				SendImages: r.FormValue("st_"+st+"_send_images") != "",
			}
			if t := strings.TrimSpace(r.FormValue("st_" + st + "_temperature")); t != "" {
				v, err := strconv.ParseFloat(t, 64)
				if err != nil {
					return fmt.Errorf("%s temperature: %v", st, err)
				}
				sm.Temperature = &v
			}
			if mt := strings.TrimSpace(r.FormValue("st_" + st + "_max_tokens")); mt != "" {
				v, err := strconv.Atoi(mt)
				if err != nil {
					return fmt.Errorf("%s max tokens: %v", st, err)
				}
				sm.MaxTokens = v
			}
			c.Stages[st] = sm
		}
		return nil
	})
	settingsDone(w, r, "llm", err)
}

// handleSaveImage stores the image generation settings.
func (s *Server) handleSaveImage(w http.ResponseWriter, r *http.Request) {
	err := s.cfg.Update(func(c *config.Config) error {
		c.Image = config.ImageConfig{
			Provider:    r.FormValue("img_provider"),
			Model:       strings.TrimSpace(r.FormValue("img_model")),
			RefModel:    strings.TrimSpace(r.FormValue("img_ref_model")),
			AspectRatio: r.FormValue("img_aspect"),
		}
		if ex := strings.TrimSpace(r.FormValue("img_extra")); ex != "" {
			if err := json.Unmarshal([]byte(ex), &c.Image.Extra); err != nil {
				return fmt.Errorf("extra image parameters must be a JSON object: %v", err)
			}
		}
		return nil
	})
	settingsDone(w, r, "image", err)
}

// ---------------------------------------------------------------------------
// Prompt templates

type promptView struct {
	Name       string
	Source     string
	Overridden bool
}

func (s *Server) handlePrompts(w http.ResponseWriter, r *http.Request) {
	var list []promptView
	for _, n := range s.prompts.Names() {
		src, over, err := s.prompts.Source(n)
		if err != nil {
			src = "error: " + err.Error()
		}
		list = append(list, promptView{Name: n, Source: src, Overridden: over})
	}
	s.render(w, r, "prompts", view{Title: "Prompt templates", Nav: "prompts", D: map[string]any{
		"List": list, "Dir": s.prompts.OverrideDir,
	}})
}

func (s *Server) handleSavePrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	src := strings.ReplaceAll(r.FormValue("source"), "\r\n", "\n")
	if def, _ := s.prompts.Default(name); def == src {
		done(w, r, "/prompts#p-"+name, s.prompts.Reset(name))
		return
	}
	done(w, r, "/prompts#p-"+name, s.prompts.Save(name, src))
}

func (s *Server) handleResetPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	done(w, r, "/prompts#p-"+name, s.prompts.Reset(name))
}
