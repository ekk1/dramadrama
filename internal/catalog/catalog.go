// Package catalog keeps the model lists fetched from providers, so model
// IDs can be picked instead of typed.
//
// Each provider's list is one file, <dir>/<provider>.json, replaced
// whenever the list is fetched again. Nothing is fetched automatically.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
)

// Model is one model offered by a provider.
type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Category    string `json:"category,omitempty"` // media providers: "text-to-image", "image-to-video", …
	Description string `json:"description,omitempty"`
}

// List is the cached model list of one provider.
type List struct {
	Provider  string    `json:"provider"`
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

// Store reads and writes the cached lists. A nil *Store has no lists.
type Store struct {
	dir string
}

// Open returns a store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// path maps a provider name to its file. Names are user-chosen, so they
// are escaped rather than trusted as file names.
func (s *Store) path(provider string) string {
	return filepath.Join(s.dir, url.PathEscape(provider)+".json")
}

// Get returns the cached list of a provider, or nil if there is none.
func (s *Store) Get(provider string) *List {
	if s == nil || provider == "" {
		return nil
	}
	data, err := os.ReadFile(s.path(provider))
	if err != nil {
		return nil
	}
	var l List
	if json.Unmarshal(data, &l) != nil {
		return nil
	}
	return &l
}

// Save replaces the cached list of a provider.
func (s *Store) Save(provider string, models []Model) (*List, error) {
	if s == nil {
		return nil, errors.New("catalog: no store")
	}
	l := &List{Provider: provider, FetchedAt: time.Now().UTC(), Models: models}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return l, assets.WriteFileAtomic(s.path(provider), data)
}

// Delete removes a provider's cached list.
func (s *Store) Delete(provider string) {
	if s != nil && provider != "" {
		os.Remove(s.path(provider))
	}
}

// Rename moves a provider's cached list to a new provider name.
func (s *Store) Rename(from, to string) {
	if s == nil || from == "" || to == "" || from == to {
		return
	}
	if l := s.Get(from); l != nil {
		if _, err := s.Save(to, l.Models); err == nil {
			s.Delete(from)
		}
	}
}

// Fetch asks a provider for its models over the network.
func Fetch(ctx context.Context, p *config.Provider) ([]Model, error) {
	switch {
	case p.IsLLM():
		list, err := llm.ListModels(ctx, llm.Options{Type: p.Type, BaseURL: p.BaseURL, APIKey: p.Key(), Headers: p.Headers})
		if err != nil {
			return nil, err
		}
		out := make([]Model, len(list))
		for i, m := range list {
			out[i] = Model{ID: m.ID, Name: m.Name, Description: m.Description}
		}
		return out, nil
	case p.IsImage():
		list, err := imagegen.ListModels(ctx, imagegen.Options{
			Type: p.Type, BaseURL: p.BaseURL, APIKey: p.Key(), Headers: p.Headers, CatalogURL: p.CatalogURL,
		})
		if err != nil {
			return nil, err
		}
		out := make([]Model, len(list))
		for i, m := range list {
			out[i] = Model{ID: m.ID, Name: m.Name, Category: m.Category, Description: m.Description}
		}
		return out, nil
	}
	return nil, fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
}
