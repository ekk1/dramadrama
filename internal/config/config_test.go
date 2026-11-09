package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCreatesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Errorf("config file should not be group/world readable: %v", st.Mode())
	}
	c := m.Get()
	if c.Provider("fal") == nil || c.Stages[StageIdea].Model == "" {
		t.Errorf("defaults missing: %+v", c)
	}
	c.Providers = append(c.Providers, Provider{Name: "x", Type: "openai_chat", APIKeyEnv: "DD_TEST_KEY"})
	c.Stages[StageIdea] = StageModel{Provider: "x", Model: "m"}
	if err := m.Save(c); err != nil {
		t.Fatal(err)
	}
	m2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DD_TEST_KEY", "abc")
	if p := m2.Get().Provider("x"); p == nil || p.Key() != "abc" {
		t.Errorf("env key not resolved: %+v", p)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Stages[StageIdea] = StageModel{Provider: "fal", Model: "x"}
	if err := c.Validate(); err == nil {
		t.Error("image provider accepted for LLM stage")
	}
	c = Default()
	c.Providers = append(c.Providers, Provider{Name: "fal", Type: "fal"})
	if err := c.Validate(); err == nil {
		t.Error("duplicate provider accepted")
	}
	c = Default()
	c.Image.Provider = "anthropic"
	if err := c.Validate(); err == nil {
		t.Error("LLM provider accepted for images")
	}
}
