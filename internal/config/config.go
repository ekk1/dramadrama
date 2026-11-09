// Package config loads and saves the workbench configuration: provider
// endpoints and credentials, and which provider/model each stage uses.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/imagegen"
	"github.com/ekk1/dramadrama/internal/llm"
)

// LLM stages that have a model setting.
const (
	StageIdea          = "idea"
	StageVisualPrompts = "visual_prompts"
	StageStoryboard    = "storyboard"
	StageShotPrompts   = "shot_prompts"
)

// LLMStages lists the LLM-backed stages in pipeline order.
var LLMStages = []string{StageIdea, StageVisualPrompts, StageStoryboard, StageShotPrompts}

// Config is the whole configuration file.
type Config struct {
	Providers []Provider            `json:"providers"`
	Stages    map[string]StageModel `json:"stages"`
	Image     ImageConfig           `json:"image"`
}

// Provider is one API endpoint.
type Provider struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // see llm.Types and imagegen.Types
	BaseURL string `json:"base_url,omitempty"`
	// APIKey is stored in the config file; APIKeyEnv names an environment
	// variable to read instead. APIKey wins when both are set.
	APIKey    string            `json:"api_key,omitempty"`
	APIKeyEnv string            `json:"api_key_env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Key resolves the provider's API key.
func (p *Provider) Key() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	if p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return ""
}

// HasKey reports whether a key is configured (directly or via env).
func (p *Provider) HasKey() bool { return p.Key() != "" }

// IsLLM reports whether the provider is a language model API.
func (p *Provider) IsLLM() bool { return slices.Contains(llm.Types, p.Type) }

// IsImage reports whether the provider is an image generation API.
func (p *Provider) IsImage() bool { return slices.Contains(imagegen.Types, p.Type) }

// StageModel selects the model for one LLM stage.
type StageModel struct {
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	// SendImages attaches the project's reference images to the request
	// (requires a vision-capable model).
	SendImages bool `json:"send_images,omitempty"`
}

// ImageConfig selects the image backend.
type ImageConfig struct {
	Provider string `json:"provider"`
	// Model is used for text-only generation.
	Model string `json:"model"`
	// RefModel is used when reference images are attached.
	RefModel    string         `json:"ref_model"`
	AspectRatio string         `json:"aspect_ratio"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// Default returns the configuration written on first start.
func Default() *Config {
	return &Config{
		Providers: []Provider{
			{Name: "anthropic", Type: llm.TypeAnthropic, APIKeyEnv: "ANTHROPIC_API_KEY"},
			{Name: "openai", Type: llm.TypeOpenAIResponses, APIKeyEnv: "OPENAI_API_KEY"},
			{Name: "openai-compatible", Type: llm.TypeOpenAIChat, BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
			{Name: "gemini", Type: llm.TypeGemini, APIKeyEnv: "GEMINI_API_KEY"},
			{Name: "fal", Type: imagegen.TypeFal, APIKeyEnv: "FAL_KEY"},
		},
		Stages: map[string]StageModel{
			StageIdea:          {Provider: "anthropic", Model: "claude-opus-5-5"},
			StageVisualPrompts: {Provider: "anthropic", Model: "claude-opus-5-5", SendImages: true},
			StageStoryboard:    {Provider: "anthropic", Model: "claude-opus-5-5"},
			StageShotPrompts:   {Provider: "anthropic", Model: "claude-opus-5-5"},
		},
		Image: ImageConfig{
			Provider:    "fal",
			Model:       "fal-ai/flux/dev",
			RefModel:    "fal-ai/flux-pro/kontext",
			AspectRatio: "9:16",
		},
	}
}

// Provider looks up a provider by name.
func (c *Config) Provider(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

// Validate checks internal consistency.
func (c *Config) Validate() error {
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("provider name is required")
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
		if !p.IsLLM() && !p.IsImage() {
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
	}
	for stage, sm := range c.Stages {
		if sm.Provider == "" {
			continue
		}
		p := c.Provider(sm.Provider)
		if p == nil {
			return fmt.Errorf("stage %s: unknown provider %q", stage, sm.Provider)
		}
		if !p.IsLLM() {
			return fmt.Errorf("stage %s: provider %q is not an LLM provider", stage, sm.Provider)
		}
	}
	if c.Image.Provider != "" {
		p := c.Provider(c.Image.Provider)
		if p == nil || !p.IsImage() {
			return fmt.Errorf("image: %q is not an image provider", c.Image.Provider)
		}
	}
	return nil
}

func (c *Config) clone() *Config {
	data, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(data, &out)
	if out.Stages == nil {
		out.Stages = map[string]StageModel{}
	}
	return &out
}

// Manager guards the config file.
type Manager struct {
	path string
	mu   sync.RWMutex
	cfg  *Config
}

// Load reads path, creating it with defaults if it does not exist.
func Load(path string) (*Manager, error) {
	m := &Manager{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		m.cfg = Default()
		return m, m.write(m.cfg)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.Stages == nil {
		c.Stages = map[string]StageModel{}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	m.cfg = &c
	return m, nil
}

// Get returns a copy of the current configuration.
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.clone()
}

// Save validates and persists a new configuration.
func (m *Manager) Save(c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.write(c); err != nil {
		return err
	}
	m.cfg = c.clone()
	return nil
}

// write persists the file; it is created with 0600 permissions because it
// may contain API keys.
func (m *Manager) write(c *Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return assets.WriteFileAtomic(m.path, data)
}
