package imagegen

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Video models name their inputs differently: the first frame may be
// image_url, first_frame_image_url or start_image_url, the duration may be
// an integer or a string enum, and so on. Rather than hard-code each
// model, the request describes its inputs by role and the roles are
// matched against the model's declared input schema.

// Roles of a video request's inputs.
const (
	RolePrompt     = "prompt"
	RoleFirstFrame = "first_frame"
	RoleLastFrame  = "last_frame"
	RoleRefImages  = "reference_images"
	RoleRefVideos  = "reference_videos"
	RoleRefAudios  = "reference_audios"
	RoleDuration   = "duration"
	RoleAspect     = "aspect_ratio"
)

// Roles lists all roles in display order.
var Roles = []string{
	RolePrompt, RoleFirstFrame, RoleLastFrame, RoleRefImages, RoleRefVideos, RoleRefAudios, RoleDuration, RoleAspect,
}

// Param is one input field a model declares.
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type,omitempty"` // string, integer, number, boolean, array, object
	Enum        []string `json:"enum,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
}

// InputSchema is the list of inputs a model declares.
type InputSchema struct {
	Model  string  `json:"model"`
	Params []Param `json:"params"`
}

// Param returns the named input, or nil.
func (s *InputSchema) Param(name string) *Param {
	if s == nil {
		return nil
	}
	for i := range s.Params {
		if s.Params[i].Name == name {
			return &s.Params[i]
		}
	}
	return nil
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// roleOf guesses which role an input name plays, or "".
func roleOf(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "prompt":
		return RolePrompt
	case containsAny(n, "last_frame", "end_frame", "end_image", "tail_image", "tail_frame", "last_image"):
		return RoleLastFrame
	case containsAny(n, "first_frame", "start_frame", "start_image", "first_image"):
		return RoleFirstFrame
	case containsAny(n, "reference_image", "ref_image", "subject_image") || n == "image_urls" || n == "images":
		return RoleRefImages
	case containsAny(n, "reference_video", "ref_video", "motion_video") || n == "video_urls" || n == "video_url" || n == "videos":
		return RoleRefVideos
	case containsAny(n, "reference_audio", "ref_audio") || n == "audio_urls" || n == "audio_url" || n == "audios":
		return RoleRefAudios
	case n == "duration" || n == "duration_seconds" || n == "seconds":
		return RoleDuration
	case n == "aspect_ratio":
		return RoleAspect
	}
	return ""
}

// ResolveRoles maps each role to the model input that carries it.
//
// Roles are detected from the schema's input names; override (role →
// field name) wins. Without a schema only the two near-universal
// conventions are assumed: "prompt" and "image_url" for the first frame.
func ResolveRoles(schema *InputSchema, override map[string]string) map[string]Param {
	out := map[string]Param{}
	if schema == nil || len(schema.Params) == 0 {
		out[RolePrompt] = Param{Name: "prompt", Type: "string"}
		out[RoleFirstFrame] = Param{Name: "image_url", Type: "string"}
	} else {
		for _, p := range schema.Params {
			role := roleOf(p.Name)
			if role == "" {
				continue
			}
			// The first match wins, except that a URL field beats a
			// non-URL field of the same role.
			if cur, ok := out[role]; ok && (strings.Contains(cur.Name, "url") || !strings.Contains(p.Name, "url")) {
				continue
			}
			out[role] = p
		}
		// A lone image input is the first frame.
		if _, ok := out[RoleFirstFrame]; !ok {
			for _, name := range []string{"image_url", "image"} {
				if p := schema.Param(name); p != nil {
					out[RoleFirstFrame] = *p
					break
				}
			}
		}
	}
	for role, field := range override {
		field = strings.TrimSpace(field)
		if !slices.Contains(Roles, role) {
			continue
		}
		if field == "" || field == "-" { // explicitly not sent
			delete(out, role)
			continue
		}
		if p := schema.Param(field); p != nil {
			out[role] = *p
			continue
		}
		p := Param{Name: field, Type: "string"}
		if strings.HasSuffix(field, "s") && role != RoleDuration && role != RoleAspect && role != RolePrompt {
			p.Type = "array"
		}
		out[role] = p
	}
	return out
}

// leadingNumber parses the number at the start of s ("8s" → 8).
func leadingNumber(s string) (float64, bool) {
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.') {
		end++
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	return v, err == nil
}

// durationValue converts seconds to the value the input accepts: the
// nearest enum entry, or a number clamped to the declared range.
func durationValue(p Param, seconds float64) any {
	if len(p.Enum) > 0 {
		best, bestDiff := "", math.Inf(1)
		for _, e := range p.Enum {
			if v, ok := leadingNumber(e); ok {
				if d := math.Abs(v - seconds); d < bestDiff {
					best, bestDiff = e, d
				}
			}
		}
		if best == "" {
			return nil
		}
		if p.Type == "integer" || p.Type == "number" {
			v, _ := leadingNumber(best)
			return v
		}
		return best
	}
	if p.Min != nil {
		seconds = math.Max(seconds, *p.Min)
	}
	if p.Max != nil {
		seconds = math.Min(seconds, *p.Max)
	}
	switch p.Type {
	case "integer":
		return int(math.Round(seconds))
	case "number":
		return seconds
	}
	return strconv.Itoa(int(math.Round(seconds)))
}

// ParseOpenAPI extracts a model's inputs from the OpenAPI 3 document fal
// publishes for it: the schema of the POST request body.
func ParseOpenAPI(model string, doc json.RawMessage) (*InputSchema, error) {
	type prop struct {
		Type        any               `json:"type"`
		Enum        []any             `json:"enum"`
		AnyOf       []json.RawMessage `json:"anyOf"`
		AllOf       []json.RawMessage `json:"allOf"`
		Ref         string            `json:"$ref"`
		Description string            `json:"description"`
		Minimum     *float64          `json:"minimum"`
		Maximum     *float64          `json:"maximum"`
	}
	var d struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("imagegen: bad OpenAPI document: %w", err)
	}
	refName := func(ref string) string { return ref[strings.LastIndexByte(ref, '/')+1:] }

	// The input schema is the body of the (only) POST operation; fall
	// back to a component named "…Input".
	name := ""
	paths := make([]string, 0, len(d.Paths))
	for p := range d.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if op, ok := d.Paths[p]["post"]; ok {
			if ref := op.RequestBody.Content["application/json"].Schema.Ref; ref != "" {
				name = refName(ref)
				break
			}
		}
	}
	if _, ok := d.Components.Schemas[name]; !ok {
		name = ""
		for n := range d.Components.Schemas {
			if strings.HasSuffix(n, "Input") && (name == "" || len(n) < len(name)) {
				name = n
			}
		}
	}
	if name == "" {
		return nil, errors.New("imagegen: no input schema in the OpenAPI document")
	}
	var in struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
		Order      []string                   `json:"x-fal-order-properties"`
	}
	if err := json.Unmarshal(d.Components.Schemas[name], &in); err != nil {
		return nil, fmt.Errorf("imagegen: bad input schema: %w", err)
	}

	// resolve flattens $ref / anyOf / allOf wrappers, ignoring "null".
	var resolve func(raw json.RawMessage, depth int) prop
	resolve = func(raw json.RawMessage, depth int) prop {
		var p prop
		if json.Unmarshal(raw, &p) != nil || depth > 4 {
			return p
		}
		merge := func(sub prop) {
			if p.Type == nil {
				p.Type = sub.Type
			}
			if len(p.Enum) == 0 {
				p.Enum = sub.Enum
			}
			if p.Minimum == nil {
				p.Minimum = sub.Minimum
			}
			if p.Maximum == nil {
				p.Maximum = sub.Maximum
			}
		}
		if p.Ref != "" {
			if target, ok := d.Components.Schemas[refName(p.Ref)]; ok {
				merge(resolve(target, depth+1))
			}
		}
		for _, alt := range append(p.AnyOf, p.AllOf...) {
			sub := resolve(alt, depth+1)
			if t, _ := sub.Type.(string); t == "null" {
				continue
			}
			merge(sub)
		}
		return p
	}

	names := in.Order
	for n := range in.Properties {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(in.Order) == 0 {
		sort.Strings(names)
	}
	out := &InputSchema{Model: model}
	for _, n := range names {
		raw, ok := in.Properties[n]
		if !ok {
			continue
		}
		p := resolve(raw, 0)
		param := Param{Name: n, Description: p.Description, Min: p.Minimum, Max: p.Maximum, Required: slices.Contains(in.Required, n)}
		switch t := p.Type.(type) {
		case string:
			param.Type = t
		case []any: // ["string", "null"]
			for _, x := range t {
				if s, _ := x.(string); s != "" && s != "null" {
					param.Type = s
					break
				}
			}
		}
		for _, e := range p.Enum {
			if e != nil {
				param.Enum = append(param.Enum, fmt.Sprint(e))
			}
		}
		out.Params = append(out.Params, param)
	}
	if len(out.Params) == 0 {
		return nil, errors.New("imagegen: the model declares no inputs")
	}
	return out, nil
}
