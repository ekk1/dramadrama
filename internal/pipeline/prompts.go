package pipeline

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
