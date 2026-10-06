package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is a rules YAML document in the PRD shape. The database stays the
// source of truth; this is import/export only.
type File struct {
	Defaults Defaults
	Rules    []Rule
}

// Defaults is the optional "defaults" block. The models are daemon settings,
// so the importer decides what to do with them; MinConfidence is copied onto
// every rule that sets none.
type Defaults struct {
	DecisionModel string   `yaml:"decision_model,omitempty"`
	FallbackModel string   `yaml:"fallback_model,omitempty"`
	MinConfidence *float64 `yaml:"min_confidence,omitempty"`
}

type yamlFile struct {
	Defaults Defaults   `yaml:"defaults,omitempty"`
	Rules    []yamlRule `yaml:"rules"`
}

// yamlRule is one rule as written in the file. id is the rule's name; stack
// and enabled are additions to the PRD shape so an export loses nothing.
type yamlRule struct {
	ID            string   `yaml:"id"`
	Said          string   `yaml:"said,omitempty"`
	When          string   `yaml:"when,omitempty"`   // intent
	Match         any      `yaml:"match,omitempty"`  // conditions: tree or map shorthand
	Unless        any      `yaml:"unless,omitempty"` // exceptions: tree or map shorthand
	Actions       []any    `yaml:"actions"`          // "move:Food" or {type, folder}
	MinConfidence *float64 `yaml:"min_confidence,omitempty"`
	Model         string   `yaml:"model,omitempty"`
	Stack         bool     `yaml:"stack,omitempty"`
	Enabled       *bool    `yaml:"enabled,omitempty"` // omitted = true
}

// ParseYAML reads a rules file and validates every rule. Priority follows
// the order in the file. All problems are returned together, each prefixed
// with its rule's id.
func ParseYAML(data []byte) (File, error) {
	var yf yamlFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&yf); err != nil {
		return File{}, fmt.Errorf("parse rules yaml: %w", err)
	}
	f := File{Defaults: yf.Defaults}
	var errs []error
	seen := map[string]bool{}
	for i, yr := range yf.Rules {
		r, err := yr.rule()
		if err == nil {
			r.Priority = i + 1
			if r.MinConfidence == nil {
				r.MinConfidence = yf.Defaults.MinConfidence
			}
			err = r.Validate()
		}
		if err == nil && seen[r.Name] {
			err = &ValidationError{"id", "used by more than one rule"}
		}
		seen[r.Name] = true
		if err != nil {
			errs = append(errs, fmt.Errorf("rule %d (%q): %w", i+1, yr.ID, err))
		}
		f.Rules = append(f.Rules, r)
	}
	if len(errs) > 0 {
		return File{}, errors.Join(errs...)
	}
	return f, nil
}

func (yr yamlRule) rule() (Rule, error) {
	r := Rule{Name: yr.ID, Said: yr.Said, Intent: yr.When, MinConfidence: yr.MinConfidence,
		Model: yr.Model, Stack: yr.Stack, Enabled: yr.Enabled == nil || *yr.Enabled}
	var err error
	if r.Conditions, err = condFromYAML(yr.Match); err != nil {
		return r, &ValidationError{"conditions", err.Error()}
	}
	if r.Exceptions, err = condFromYAML(yr.Unless); err != nil {
		return r, &ValidationError{"exceptions", err.Error()}
	}
	for i, item := range yr.Actions {
		var a Action
		switch v := item.(type) {
		case string:
			a.Type, a.Folder, _ = strings.Cut(v, ":")
		case map[string]any:
			b, err := json.Marshal(v)
			if err == nil {
				dec := json.NewDecoder(bytes.NewReader(b))
				dec.DisallowUnknownFields()
				err = dec.Decode(&a)
			}
			if err != nil {
				return r, &ValidationError{fmt.Sprintf("actions[%d]", i), "needs type and, for move, folder"}
			}
		default:
			return r, &ValidationError{fmt.Sprintf("actions[%d]", i), `write an action as "move:Folder", "trash" or {type, folder}`}
		}
		r.Actions = append(r.Actions, a)
	}
	return r, nil
}

// condFromYAML reads a condition tree, or the shorthand where a plain map
// such as {from_domain: [a, b], is_bulk: true} means "all of these fields
// equal or are in these values".
func condFromYAML(v any) (Cond, error) {
	if v == nil {
		return Cond{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Cond{}, errors.New("must be a map")
	}
	tree := false
	for _, k := range []string{"all", "any", "field"} {
		if _, ok := m[k]; ok {
			tree = true
		}
	}
	if !tree {
		fields := make([]string, 0, len(m))
		for k := range m {
			fields = append(fields, k)
		}
		slices.Sort(fields)
		leaves := make([]any, 0, len(m))
		for _, k := range fields {
			op := OpEq
			if _, ok := m[k].([]any); ok {
				op = OpIn
			}
			leaves = append(leaves, map[string]any{"field": k, "op": op, "value": m[k]})
		}
		v = map[string]any{"all": leaves}
	}
	// Through JSON so numbers, lists and regexes look exactly like a stored tree.
	b, err := json.Marshal(v)
	if err != nil {
		return Cond{}, fmt.Errorf("encode condition: %w", err)
	}
	var c Cond
	if err := json.Unmarshal(b, &c); err != nil {
		return Cond{}, err
	}
	return c, nil
}

// condToYAML writes the shorthand when the tree is an "all" of eq/in leaves
// on distinct fields, and the full tree otherwise.
func condToYAML(c Cond) (any, error) {
	if c.IsEmpty() {
		return nil, nil
	}
	short := map[string]any{}
	for _, ch := range c.All {
		_, isList := list(ch.Value)
		_, dup := short[ch.Field]
		if ch.Field == "" || dup || (ch.Op != OpEq || isList) && (ch.Op != OpIn || !isList) {
			short = nil
			break
		}
		short[ch.Field] = ch.Value
	}
	if len(short) > 0 {
		return short, nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode condition: %w", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("encode condition: %w", err)
	}
	return out, nil
}

// MarshalYAML writes a rules file in priority order, using the shorthands.
func MarshalYAML(f File) ([]byte, error) {
	rs := slices.Clone(f.Rules)
	slices.SortStableFunc(rs, func(a, b Rule) int { return a.Priority - b.Priority })
	yf := yamlFile{Defaults: f.Defaults, Rules: make([]yamlRule, 0, len(rs))}
	for _, r := range rs {
		yr := yamlRule{ID: r.Name, Said: r.Said, When: r.Intent, MinConfidence: r.MinConfidence, Model: r.Model, Stack: r.Stack}
		if !r.Enabled {
			yr.Enabled = new(bool)
		}
		var err error
		if yr.Match, err = condToYAML(r.Conditions); err != nil {
			return nil, err
		}
		if yr.Unless, err = condToYAML(r.Exceptions); err != nil {
			return nil, err
		}
		for _, a := range r.Actions {
			yr.Actions = append(yr.Actions, a.String())
		}
		yf.Rules = append(yf.Rules, yr)
	}
	out, err := yaml.Marshal(yf)
	if err != nil {
		return nil, fmt.Errorf("write rules yaml: %w", err)
	}
	return out, nil
}
