package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ekk1/dramadrama/internal/assets"
	"github.com/ekk1/dramadrama/internal/config"
	"github.com/ekk1/dramadrama/web"
)

// TestTemplateKeysTranslated fails when a template uses a string that has no
// Chinese translation.
func TestTemplateKeysTranslated(t *testing.T) {
	keyRe := []*regexp.Regexp{
		regexp.MustCompile(`\bt "((?:[^"\\]|\\.)+)"`),               // {{t "…"}}
		regexp.MustCompile("\\bt `([^`]+)`"),                        // {{t `…`}}
		regexp.MustCompile(`"(?:Label|Empty)" "((?:[^"\\]|\\.)+)"`), // strings passed to partials
	}
	files, _ := fs.Glob(web.FS, "templates/*.html")
	var missing []string
	for _, f := range files {
		data, _ := fs.ReadFile(web.FS, f)
		for _, re := range keyRe {
			for _, m := range re.FindAllStringSubmatch(string(data), -1) {
				key := strings.ReplaceAll(m[1], `\"`, `"`)
				if _, ok := zh[key]; !ok {
					missing = append(missing, f+": "+key)
				}
			}
		}
	}
	// Keys produced in Go code.
	var goKeys []string
	for _, k := range assets.Kinds {
		goKeys = append(goKeys, string(k))
	}
	goKeys = append(goKeys, config.LLMStages...)
	goKeys = append(goKeys, "character", "scene", "prop", "style", "Saved",
		"Input", "Story", "Visuals", "Storyboard", "Shot prompts", "Keyframes", "upload too large (limit 64 MB)",
		"Project deleted. Its assets remain in the library.", "interrupted (server restarted)")
	for _, k := range logKindNames {
		goKeys = append(goKeys, k)
	}
	for _, k := range stageNames {
		goKeys = append(goKeys, k)
	}
	for _, k := range roleLabels {
		goKeys = append(goKeys, k)
	}
	goKeys = append(goKeys, "Image", "Video", "Audio", "This shot has no keyframe: generating without a first frame.",
		"The request changed since the unfinished one was submitted: cancelling that one.",
		"The request is still with the provider. Run this step again to collect its result.",
		"The model's parameter list is unknown: only the prompt and the first frame are sent.")
	goKeys = append(goKeys, "ok", "error", "System prompt", "User prompt", "Prompt", "Output", "Parameters", "Details")
	goKeys = append(goKeys, jsKeys...)
	for _, k := range goKeys {
		if _, ok := zh[k]; !ok {
			missing = append(missing, "go: "+k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing zh translations:\n%s", strings.Join(missing, "\n"))
	}
}

// TestPipelineErrorsTranslated checks the fixed error messages of the
// pipeline, which are shown in the UI.
func TestPipelineErrorsTranslated(t *testing.T) {
	files, _ := filepath.Glob("../pipeline/*.go")
	if len(files) == 0 {
		t.Fatal("pipeline sources not found")
	}
	var src []byte
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, b...)
	}
	re := regexp.MustCompile(`errors\.New\("((?:[^"\\]|\\.)+)"\)`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if _, ok := zh[m[1]]; !ok {
			t.Errorf("pipeline error without zh translation: %q", m[1])
		}
	}
	reBT := regexp.MustCompile("errors\\.New\\(`([^`]+)`\\)")
	for _, m := range reBT.FindAllStringSubmatch(string(src), -1) {
		if _, ok := zh[m[1]]; !ok {
			t.Errorf("pipeline error without zh translation: %q", m[1])
		}
	}
}

func TestTranslatorPatterns(t *testing.T) {
	tr := translator(LangZH)
	cases := map[string]string{
		"Saved":                                "已保存",
		"Calling anthropic / claude-opus-5-5…": "正在调用 anthropic / claude-opus-5-5…",
		"── Shot 3 (2/8)":                      "── 镜头 3（2/8）",
		"Error: shot 4 has no keyframe prompt yet: run step 5 first":                                                     "错误：镜头 4 还没有关键帧提示词：请先运行第 5 步",
		`provider "anthropic" has no API key: set the ANTHROPIC_API_KEY environment variable or store a key in Settings`: "服务商 “anthropic” 没有 API 密钥：请设置环境变量 ANTHROPIC_API_KEY，或在设置中保存密钥",
		"llm: HTTP 500: boom": "llm: HTTP 500: boom", // unknown text passes through
		`could not parse the output as JSON (no "shots" in output); the raw text was saved so you can fix and apply it`: `无法将输出解析为 JSON（输出中没有 "shots"）；原始文本已保存，可修改后应用`,
	}
	for in, want := range cases {
		if got := tr(in); got != want {
			t.Errorf("tr(%q) = %q, want %q", in, got, want)
		}
	}
	if got := translator(LangEN)("Saved"); got != "Saved" {
		t.Errorf("en should be identity, got %q", got)
	}
}

func TestLangOf(t *testing.T) {
	cases := []struct {
		cookie, accept, want string
	}{
		{"", "", LangEN},
		{"", "zh-CN,zh;q=0.9,en;q=0.8", LangZH},
		{"", "en-US,en;q=0.9,zh;q=0.8", LangEN},
		{"en", "zh-CN", LangEN},
		{"zh", "en-US", LangZH},
		{"fr", "zh-TW", LangZH}, // invalid cookie ignored
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.cookie != "" {
			r.AddCookie(&http.Cookie{Name: langCookie, Value: c.cookie})
		}
		if c.accept != "" {
			r.Header.Set("Accept-Language", c.accept)
		}
		if got := langOf(r); got != c.want {
			t.Errorf("langOf(cookie=%q, accept=%q) = %q, want %q", c.cookie, c.accept, got, c.want)
		}
	}
}

func TestLanguageSwitch(t *testing.T) {
	h := newHarness(t)
	if page := h.get("/projects"); !strings.Contains(page, `lang="en"`) || !strings.Contains(page, "New project") {
		t.Fatal("default page should be English")
	}
	// Switching sets the cookie (the harness client keeps it) and goes back.
	resp, err := h.c.Get(h.srv.URL + "/lang/zh?back=" + "%2Fassets%3Fkind%3Dconcept")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.RequestURI() != "/assets?kind=concept" {
		t.Errorf("not redirected back: %s", resp.Request.URL)
	}
	page := h.get("/projects")
	for _, want := range []string{`lang="zh-CN"`, "新建项目", "素材库", `"Running…":"运行中…"`} {
		if !strings.Contains(page, want) {
			t.Errorf("zh page missing %q", want)
		}
	}
	// Flash messages are translated too.
	if page := h.get("/settings?msg=Saved"); !strings.Contains(page, "已保存") {
		t.Error("flash message not translated")
	}
	// Every page renders in Chinese.
	u := h.post("/projects", map[string][]string{"name": {"x"}})
	for _, p := range []string{u.Path, "/assets", "/settings", "/prompts"} {
		if page := h.get(p); !strings.Contains(page, `lang="zh-CN"`) {
			t.Errorf("%s not rendered in zh", p)
		}
	}
	// Unknown language is rejected.
	resp, _ = h.c.Get(h.srv.URL + "/lang/fr")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/lang/fr: %d", resp.StatusCode)
	}
}
