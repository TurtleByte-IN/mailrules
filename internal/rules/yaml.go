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
	// Mailboxes says which mailbox each rule applies to, by the rule's name: the mailbox's
	// address (its login), or AllMailboxes. A rule with no entry says nothing about it. A
	// file cannot hold numeric account ids, which differ from one install to the next, so
	// whoever has the accounts maps the address to Rule.AccountID and back.
	Mailboxes map[string]string
}

// AllMailboxes is the applies_to value of a rule that is not limited to one mailbox. An
// export never writes it (no applies_to means the rule applies everywhere); an import
// accepts it to widen a rule that is limited to one.
const AllMailboxes = "all"

// Defaults is the optional "defaults" block. The models are daemon settings, so the
// importer checks them against the install's (an import never changes a model);
// MinConfidence is copied onto every rule that sets none.
type Defaults struct {
	DecisionModel string   `yaml:"decision_model,omitempty"`
	FallbackModel string   `yaml:"fallback_model,omitempty"`
	MinConfidence *float64 `yaml:"min_confidence,omitempty"`
}

type yamlFile struct {
	Defaults Defaults   `yaml:"defaults,omitempty"`
	Rules    []yamlRule `yaml:"rules"`
}

// yamlRule is one rule as written in the file. id is the rule's name; template, stack
// and enabled are additions to the PRD shape so an export loses nothing.
type yamlRule struct {
	ID            string   `yaml:"id"`
	Said          string   `yaml:"said,omitempty"`
	Template      string   `yaml:"template,omitempty"` // the gallery template it was added from
	When          string   `yaml:"when,omitempty"`     // intent
	Match         any      `yaml:"match,omitempty"`    // conditions: tree or map shorthand
	Unless        any      `yaml:"unless,omitempty"`   // exceptions: tree or map shorthand
	Actions       []any    `yaml:"actions"`            // "move:Food" or {type, folder}
	MinConfidence *float64 `yaml:"min_confidence,omitempty"`
	Model         string   `yaml:"model,omitempty"`
	AppliesTo     string   `yaml:"applies_to,omitempty"` // a mailbox address or "all"; omitted = every mailbox
	Stack         bool     `yaml:"stack,omitempty"`
	Enabled       *bool    `yaml:"enabled,omitempty"` // omitted = true
}

// RuleError is a problem with one rule of a rules file: which rule, and what is wrong.
type RuleError struct {
	N   int    // the rule's place in the file, from 1
	ID  string // its id (name) as written
	Err *ValidationError
}

// Error is the line the command line prints: it keeps the path.
func (e *RuleError) Error() string { return fmt.Sprintf("rule %d (%q): %v", e.N, e.ID, e.Err) }

// Sentence is the same for a person: which rule and what is wrong, without the field path.
func (e *RuleError) Sentence() string {
	return fmt.Sprintf("Rule %d (%q): %s", e.N, e.ID, e.Err.Message)
}

func (e *RuleError) Unwrap() error { return e.Err }

// ParseYAML reads a rules file and validates every rule. Priority follows
// the order in the file. All problems are returned together (errors.Join), each a
// *RuleError.
func ParseYAML(data []byte) (File, error) {
	var yf yamlFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&yf); err != nil {
		return File{}, fmt.Errorf("parse rules yaml: %w", err)
	}
	f := File{Defaults: yf.Defaults, Mailboxes: map[string]string{}}
	var errs []error
	seen := map[string]bool{}
	for i, yr := range yf.Rules {
		r, ve := yr.rule()
		if ve == nil {
			r.Priority = i + 1
			if r.MinConfidence == nil {
				r.MinConfidence = yf.Defaults.MinConfidence
			}
			if err := r.Validate(); err != nil {
				ve = err.(*ValidationError) //nolint:errorlint // Validate returns nothing else
			}
		}
		if ve == nil && seen[r.Name] {
			ve = &ValidationError{"id", "This name is used by more than one rule."}
		}
		seen[r.Name] = true
		if to := strings.TrimSpace(yr.AppliesTo); to != "" && ve == nil {
			f.Mailboxes[r.Name] = to
		}
		if ve != nil {
			errs = append(errs, &RuleError{N: i + 1, ID: yr.ID, Err: ve})
		}
		f.Rules = append(f.Rules, r)
	}
	if len(errs) > 0 {
		return File{}, errors.Join(errs...)
	}
	return f, nil
}

func (yr yamlRule) rule() (Rule, *ValidationError) {
	r := Rule{Name: yr.ID, Said: yr.Said, Template: yr.Template, Intent: yr.When, MinConfidence: yr.MinConfidence,
		Model: yr.Model, Stack: yr.Stack, Enabled: yr.Enabled == nil || *yr.Enabled}
	var err error
	if r.Conditions, err = condFromYAML(yr.Match); err != nil {
		return r, &ValidationError{"conditions", "The conditions cannot be read: " + err.Error() + "."}
	}
	if r.Exceptions, err = condFromYAML(yr.Unless); err != nil {
		return r, &ValidationError{"exceptions", "The exceptions cannot be read: " + err.Error() + "."}
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
				return r, &ValidationError{fmt.Sprintf("actions[%d]", i), "An action needs a type and, for move, a folder."}
			}
		default:
			return r, &ValidationError{fmt.Sprintf("actions[%d]", i), `Write an action as "move:Folder", "trash" or {type, folder}.`}
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

// MarshalYAML writes a rules file in priority order, using the shorthands. A rule limited to
// a mailbox (AccountID set) needs its address in f.Mailboxes, or the file would not say so.
func MarshalYAML(f File) ([]byte, error) {
	rs := slices.Clone(f.Rules)
	slices.SortStableFunc(rs, func(a, b Rule) int { return a.Priority - b.Priority })
	yf := yamlFile{Defaults: f.Defaults, Rules: make([]yamlRule, 0, len(rs))}
	for _, r := range rs {
		yr := yamlRule{ID: r.Name, Said: r.Said, Template: r.Template, When: r.Intent, MinConfidence: r.MinConfidence, Model: r.Model, Stack: r.Stack,
			AppliesTo: f.Mailboxes[r.Name]}
		if yr.AppliesTo == "" && r.AccountID != 0 {
			// Dropping it would write a file that widens the rule to every mailbox.
			return nil, fmt.Errorf("rule %q applies to mailbox %d, which the file has no address for", r.Name, r.AccountID)
		}
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
