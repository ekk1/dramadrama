package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/ekk1/dramadrama/internal/project"
	"github.com/ekk1/dramadrama/internal/reqlog"
)

const logsPerPage = 100

type logsView struct {
	Days     []reqlog.Day
	Day      string
	Entries  []*reqlog.Entry
	Projects map[string]string
	Total    int // entries of the day that match the filters
	Size     int64
	// Filters.
	Kind, Status, Project string
	Page, Pages           int
	// Query is the filter part of the URL, for paging and day links.
	Query string
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := logsView{Kind: q.Get("kind"), Status: q.Get("status"), Project: q.Get("project"), Projects: s.projectNames()}
	days, err := s.logs.Days()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v.Days = days
	for _, d := range days {
		v.Size += d.Size
	}
	v.Day = q.Get("day")
	if v.Day == "" && len(days) > 0 {
		v.Day = days[0].Date
	}
	all, err := s.logs.List(v.Day)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	all = slices.DeleteFunc(all, func(e *reqlog.Entry) bool {
		return (v.Kind != "" && e.Kind != v.Kind) || (v.Status != "" && e.Status != v.Status) ||
			(v.Project != "" && e.ProjectID != v.Project)
	})
	v.Total = len(all)
	v.Pages = max(1, (len(all)+logsPerPage-1)/logsPerPage)
	v.Page, _ = strconv.Atoi(q.Get("page"))
	v.Page = min(max(v.Page, 1), v.Pages)
	v.Entries = all[min((v.Page-1)*logsPerPage, len(all)):min(v.Page*logsPerPage, len(all))]

	fq := url.Values{}
	for k, val := range map[string]string{"kind": v.Kind, "status": v.Status, "project": v.Project} {
		if val != "" {
			fq.Set(k, val)
		}
	}
	v.Query = fq.Encode()
	s.render(w, r, "logs", view{Title: "Request log", Nav: "logs", D: v})
}

// logField is one block of a log entry's detail page.
type logField struct {
	Label string // translation key
	Text  string
	Code  bool // structured data rather than prose
}

type logView struct {
	*reqlog.Entry
	Project  string
	Request  []logField
	Response []logField
	Inputs   []string // asset IDs sent with the request
	Outputs  []string // asset IDs produced
}

// logFields splits a request/response map into prose blocks for the known
// text fields, in a fixed order, and one JSON block for everything else.
func logFields(m map[string]any, rest string, texts ...[2]string) []logField {
	var out []logField
	other := map[string]any{}
	for k, v := range m {
		other[k] = v
	}
	for _, t := range texts {
		if str, ok := m[t[0]].(string); ok {
			delete(other, t[0])
			if str != "" {
				out = append(out, logField{Label: t[1], Text: str})
			}
		}
	}
	if len(other) > 0 {
		b, _ := json.MarshalIndent(other, "", "  ")
		out = append(out, logField{Label: rest, Text: string(b), Code: true})
	}
	return out
}

func stringList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if str, ok := x.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	day, name := r.PathValue("day"), r.PathValue("name")
	if r.URL.Query().Get("raw") != "" {
		data, err := s.logs.Raw(day, name)
		if logNotFound(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s_%s.json"`, day, name))
		w.Write(data)
		return
	}
	e, err := s.logs.Get(day, name)
	if logNotFound(w, err) {
		return
	}
	v := logView{Entry: e, Project: s.projectNames()[e.ProjectID]}
	v.Request = logFields(e.Request, "Parameters",
		[2]string{"system", "System prompt"}, [2]string{"user", "User prompt"}, [2]string{"prompt", "Prompt"})
	v.Response = logFields(e.Response, "Details", [2]string{"text", "Output"})
	v.Inputs = append(stringList(e.Request["images"]), stringList(e.Request["references"])...)
	v.Outputs = stringList(e.Response["assets"])
	s.render(w, r, "log", view{Title: "Request log", Nav: "logs", D: v})
}

func logNotFound(w http.ResponseWriter, err error) bool {
	if errors.Is(err, reqlog.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return true
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	return false
}

// handleDeleteLogs removes one day (day=…) or everything (all=1).
func (s *Server) handleDeleteLogs(w http.ResponseWriter, r *http.Request) {
	var err error
	switch {
	case r.FormValue("all") != "":
		err = s.logs.DeleteAll()
	default:
		err = s.logs.DeleteDay(r.FormValue("day"))
	}
	done(w, r, "/logs", err)
}

// fmtDuration renders milliseconds as "850 ms", "12.3 s" or "2m05s".
func fmtDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// logKindNames and stageNames are the display names (translation keys) of
// log kinds and pipeline stages.
var logKindNames = map[string]string{reqlog.KindLLM: "LLM", reqlog.KindImage: "Image", reqlog.KindVideo: "Video"}

var stageNames = map[string]string{
	project.StageIdea:          "Idea expansion",
	project.StageVisualPrompts: "Visual prompts",
	project.StageVisualImages:  "Concept images",
	project.StageStoryboard:    "Storyboard overview",
	project.StageShotPrompts:   "Shot-level prompts",
	project.StageKeyframes:     "Keyframes",
}

func logKind(k string) string {
	if n, ok := logKindNames[k]; ok {
		return n
	}
	return k
}

func stageName(st string) string {
	if n, ok := stageNames[st]; ok {
		return n
	}
	return st
}
