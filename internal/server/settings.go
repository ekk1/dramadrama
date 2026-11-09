package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ekk1/dramadrama/internal/config"
)

type providerView struct {
	config.Provider
	Index   int
	KeySet  bool
	EnvSet  bool
	Headers string
}

type settingsView struct {
	Providers []providerView
	Stages    map[string]config.StageModel
	Image     config.ImageConfig
	Extra     string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	v := settingsView{Stages: c.Stages, Image: c.Image}
	for i, p := range c.Providers {
		pv := providerView{Provider: p, Index: i, KeySet: p.APIKey != "", EnvSet: p.APIKeyEnv != "" && p.HasKey()}
		pv.APIKey = "" // never render secrets
		var hs []string
		for k, val := range p.Headers {
			hs = append(hs, k+": "+val)
		}
		slices.Sort(hs)
		pv.Headers = strings.Join(hs, "\n")
		v.Providers = append(v.Providers, pv)
	}
	if len(c.Image.Extra) > 0 {
		b, _ := json.MarshalIndent(c.Image.Extra, "", "  ")
		v.Extra = string(b)
	}
	s.render(w, r, "settings", view{Title: "Settings", Nav: "settings", D: v})
}

// handleSaveSettings rebuilds the config from the form. Provider rows are
// posted as parallel arrays; an empty API key field keeps the stored key.
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		done(w, r, "/settings", err)
		return
	}
	old := s.cfg.Get()
	f := r.PostForm
	c := &config.Config{Stages: map[string]config.StageModel{}}

	names, orig := f["p_name"], f["p_orig"]
	renamed := map[string]string{} // old provider name -> new name
	for i := range names {
		name := strings.TrimSpace(names[i])
		idx := strconv.Itoa(i)
		if name == "" || slices.Contains(f["p_delete"], idx) {
			continue
		}
		p := config.Provider{
			Name:      name,
			Type:      at(f["p_type"], i),
			BaseURL:   strings.TrimSpace(at(f["p_base_url"], i)),
			APIKeyEnv: strings.TrimSpace(at(f["p_key_env"], i)),
		}
		if key := strings.TrimSpace(at(f["p_key"], i)); key != "" {
			p.APIKey = key
		} else if o := old.Provider(at(orig, i)); o != nil && !slices.Contains(f["p_key_clear"], idx) {
			p.APIKey = o.APIKey
		}
		for _, line := range strings.Split(at(f["p_headers"], i), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(k) != "" {
				if p.Headers == nil {
					p.Headers = map[string]string{}
				}
				p.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		c.Providers = append(c.Providers, p)
		if o := at(orig, i); o != "" && o != name {
			renamed[o] = name
		}
	}
	// The stage selects were rendered with the old names; follow renames.
	follow := func(name string) string {
		if n, ok := renamed[name]; ok {
			return n
		}
		return name
	}

	for _, st := range config.LLMStages {
		sm := config.StageModel{
			Provider:   follow(f.Get("st_" + st + "_provider")),
			Model:      strings.TrimSpace(f.Get("st_" + st + "_model")),
			SendImages: f.Get("st_"+st+"_send_images") != "",
		}
		if t := strings.TrimSpace(f.Get("st_" + st + "_temperature")); t != "" {
			v, err := strconv.ParseFloat(t, 64)
			if err != nil {
				done(w, r, "/settings", fmt.Errorf("%s temperature: %v", st, err))
				return
			}
			sm.Temperature = &v
		}
		if mt := strings.TrimSpace(f.Get("st_" + st + "_max_tokens")); mt != "" {
			v, err := strconv.Atoi(mt)
			if err != nil {
				done(w, r, "/settings", fmt.Errorf("%s max tokens: %v", st, err))
				return
			}
			sm.MaxTokens = v
		}
		c.Stages[st] = sm
	}

	c.Image = config.ImageConfig{
		Provider:    follow(f.Get("img_provider")),
		Model:       strings.TrimSpace(f.Get("img_model")),
		RefModel:    strings.TrimSpace(f.Get("img_ref_model")),
		AspectRatio: f.Get("img_aspect"),
	}
	if ex := strings.TrimSpace(f.Get("img_extra")); ex != "" {
		if err := json.Unmarshal([]byte(ex), &c.Image.Extra); err != nil {
			done(w, r, "/settings", fmt.Errorf("extra image parameters must be a JSON object: %v", err))
			return
		}
	}
	if err := s.cfg.Save(c); err != nil {
		done(w, r, "/settings", err)
		return
	}
	done(w, r, "/settings?msg=Saved", nil)
}

func at(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return ""
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
