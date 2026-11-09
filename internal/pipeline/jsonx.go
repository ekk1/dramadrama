package pipeline

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// extractJSON decodes the JSON object in an LLM reply, tolerating Markdown
// code fences and leading/trailing prose.
func extractJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}
	// Strip a ```json … ``` fence.
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:j])), v); err == nil {
				return nil
			}
		}
	}
	// Outermost {...}.
	start, end := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return json.Unmarshal([]byte(s[start:end+1]), v)
	}
	return errors.New("no JSON object found in model output")
}

// flexFloat accepts a JSON number or a string such as "4", "4s" or "4.5 秒".
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		*f = flexFloat(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		*f = 0
		return nil
	}
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.') {
		end++
	}
	n, _ = strconv.ParseFloat(s[:end], 64)
	*f = flexFloat(n)
	return nil
}

// flexStrings accepts a JSON array of strings or a single (comma separated)
// string.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*f = cleanStrings(list)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		*f = nil
		return nil
	}
	*f = cleanStrings(strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == '、' }))
	return nil
}

func cleanStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
