package pipeline

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

//go:embed prompts/*.tmpl
var defaultPrompts embed.FS

// Prompts renders the prompt templates. Each template can be overridden by a
// file with the same name in OverrideDir; overrides are re-read on every
// render so edits apply without a restart.
type Prompts struct {
	OverrideDir string
}

var promptFuncs = template.FuncMap{
	"join": strings.Join,
}

// Source returns the active template text and whether it is overridden.
func (p *Prompts) Source(name string) (string, bool, error) {
	if p.OverrideDir != "" {
		data, err := os.ReadFile(filepath.Join(p.OverrideDir, name+".tmpl"))
		if err == nil {
			return string(data), true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
	}
	data, err := defaultPrompts.ReadFile("prompts/" + name + ".tmpl")
	if err != nil {
		return "", false, fmt.Errorf("prompt template %q not found", name)
	}
	return string(data), false, nil
}

// Render executes the named template ("idea.system", "storyboard.user", …).
func (p *Prompts) Render(name string, data any) (string, error) {
	src, _, err := p.Source(name)
	if err != nil {
		return "", err
	}
	t, err := template.New(name).Funcs(promptFuncs).Option("missingkey=zero").Parse(src)
	if err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("prompt %s: %w", name, err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// Names lists all template names ("idea.system", …) in pipeline order.
func (p *Prompts) Names() []string {
	order := []string{"idea", "visual_prompts", "storyboard", "shot_prompts"}
	var out []string
	for _, stage := range order {
		out = append(out, stage+".system", stage+".user")
	}
	return out
}

func (p *Prompts) valid(name string) bool {
	return slices.Contains(p.Names(), name)
}

// Default returns the built-in text of a template.
func (p *Prompts) Default(name string) (string, error) {
	data, err := defaultPrompts.ReadFile("prompts/" + name + ".tmpl")
	return string(data), err
}

// Save writes an override after checking that it parses.
func (p *Prompts) Save(name, src string) error {
	if !p.valid(name) {
		return fmt.Errorf("unknown prompt %q", name)
	}
	if p.OverrideDir == "" {
		return errors.New("no prompt override directory configured")
	}
	if _, err := template.New(name).Funcs(promptFuncs).Parse(src); err != nil {
		return err
	}
	if err := os.MkdirAll(p.OverrideDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.OverrideDir, name+".tmpl"), []byte(src), 0o644)
}

// Reset removes an override, restoring the built-in template.
func (p *Prompts) Reset(name string) error {
	if !p.valid(name) || p.OverrideDir == "" {
		return fmt.Errorf("unknown prompt %q", name)
	}
	err := os.Remove(filepath.Join(p.OverrideDir, name+".tmpl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
