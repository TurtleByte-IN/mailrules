package rules

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// Condition operators.
const (
	OpEq          = "eq"
	OpNe          = "ne"
	OpIn          = "in"
	OpContains    = "contains"
	OpContainsAny = "contains_any"
	OpNotContains = "not_contains"
	OpMatches     = "matches"
	OpGt          = "gt"
	OpLt          = "lt"
	OpExists      = "exists"
)

// maxRegexLen caps a "matches" pattern, in characters.
const maxRegexLen = 200

// Cond is one node of a condition tree: a group ("all" or "any" of its
// children) or a leaf (field, op, value). The zero value is the empty tree,
// which always matches.
type Cond struct {
	All   []Cond `json:"all,omitempty"`
	Any   []Cond `json:"any,omitempty"`
	Field string `json:"field,omitempty"`
	Op    string `json:"op,omitempty"`
	Value any    `json:"value,omitempty"`

	re *regexp.Regexp // compiled once on decode, for "matches" leaves
}

// UnmarshalJSON decodes a node strictly (a misspelt key must not turn a leaf
// into an empty, always-true tree) and compiles its regex once.
func (c *Cond) UnmarshalJSON(b []byte) error {
	type plain Cond // drops this method, children still use it
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("decode condition: %w", err)
	}
	*c = Cond(p)
	if c.Op == OpMatches {
		c.re, _ = compileRegex(c.Value) // a bad pattern is reported by validate
	}
	return nil
}

// IsEmpty reports whether the tree has no conditions at all.
func (c Cond) IsEmpty() bool {
	return c.Field == "" && c.Op == "" && c.Value == nil && len(c.All) == 0 && len(c.Any) == 0
}

// Match reports whether the email satisfies the tree. now is only used by
// age_days. A leaf that would not pass validation never matches.
func (c Cond) Match(e *message.Summary, now time.Time) bool {
	switch {
	case c.Field != "" || c.Op != "":
		return c.matchLeaf(e, now)
	case len(c.Any) > 0:
		return slices.ContainsFunc(c.Any, func(ch Cond) bool { return ch.Match(e, now) })
	default:
		return !slices.ContainsFunc(c.All, func(ch Cond) bool { return !ch.Match(e, now) })
	}
}

// kind is the value type of a field; it decides which operators apply.
type kind int

const (
	kindStr  kind = iota // one string or a list of them (addresses, extensions)
	kindEnum             // dmarc
	kindBool
	kindNum
	kindID // account
)

var dmarcValues = []string{"pass", "fail", "none"}

// kindOps lists the operators each kind accepts.
func kindOps(k kind) []string {
	switch k {
	case kindStr:
		return []string{OpEq, OpNe, OpIn, OpContains, OpContainsAny, OpNotContains, OpMatches, OpExists}
	case kindEnum, kindID:
		return []string{OpEq, OpNe, OpIn}
	case kindBool:
		return []string{OpEq, OpNe}
	default:
		return []string{OpEq, OpNe, OpGt, OpLt}
	}
}

// fields is the one list of condition fields: validation, the matcher and the field list
// shown to the rule composer all read it. header:<Name> is the only field not in it.
var fields = []struct {
	name string
	kind kind
}{
	{"from", kindStr}, {"to", kindStr}, {"cc", kindStr}, {"delivered_to", kindStr}, {"from_domain", kindStr},
	{"subject", kindStr}, {"body", kindStr}, {"list_id", kindStr}, {"attachment_ext", kindStr},
	{"has_attachment", kindBool}, {"is_contact", kindBool}, {"replied_before", kindBool}, {"is_bulk", kindBool}, {"is_noreply", kindBool},
	{"size_kb", kindNum}, {"age_days", kindNum}, {"dmarc", kindEnum}, {"account", kindID},
}

// fieldKind returns the kind of a known field.
func fieldKind(field string) (kind, bool) {
	for _, f := range fields {
		if f.name == field {
			return f.kind, true
		}
	}
	if name, ok := strings.CutPrefix(field, "header:"); ok && name != "" {
		return kindStr, true
	}
	return 0, false
}

// Field describes one condition field for whoever writes conditions: the rule composer's
// prompt is built from this list.
type Field struct {
	Name string
	Type string   // string | boolean | number | pass, fail or none | account id
	Ops  []string // the operators it accepts
}

// Fields lists every condition field with its type and operators, header:<Name> last.
func Fields() []Field {
	types := map[kind]string{kindStr: "string", kindBool: "boolean", kindNum: "number", kindEnum: "pass, fail or none", kindID: "account id"}
	out := make([]Field, 0, len(fields)+1)
	for _, f := range fields {
		out = append(out, Field{f.name, types[f.kind], kindOps(f.kind)})
	}
	return append(out, Field{"header:<Name>", types[kindStr], kindOps(kindStr)})
}

// ActionTypes lists the action types a rule may name.
func ActionTypes() []string { return slices.Clone(actionTypes) }

// strField returns a string field's values, lower-cased, without empties.
func strField(field string, e *message.Summary) []string {
	var vs []string
	switch field {
	case "from":
		vs = []string{e.From}
	case "to":
		vs = e.To
	case "cc":
		vs = e.Cc
	case "delivered_to":
		vs = e.DeliveredTo
	case "from_domain":
		vs = []string{e.FromDomain}
	case "subject":
		vs = []string{e.Subject}
	case "body":
		vs = []string{e.Body}
	case "list_id":
		vs = []string{e.ListID}
	case "attachment_ext":
		vs = e.AttachmentExts
	case "dmarc":
		vs = []string{e.DMARC}
	default: // header:<Name>
		vs = e.Headers[textproto.CanonicalMIMEHeaderKey(strings.TrimPrefix(field, "header:"))]
	}
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if v != "" {
			out = append(out, strings.ToLower(v))
		}
	}
	return out
}

func boolField(field string, e *message.Summary) bool {
	switch field {
	case "has_attachment":
		return e.HasAttachment
	case "is_contact":
		return e.IsContact
	case "replied_before":
		return e.RepliedBefore
	case "is_bulk":
		return e.IsBulk
	default:
		return e.IsNoreply
	}
}

func numField(field string, e *message.Summary, now time.Time) float64 {
	switch field {
	case "size_kb":
		return e.SizeKB
	case "age_days":
		return now.Sub(e.ReceivedAt).Hours() / 24
	default:
		return float64(e.AccountID)
	}
}

func (c Cond) matchLeaf(e *message.Summary, now time.Time) bool {
	k, ok := fieldKind(c.Field)
	if !ok || !slices.Contains(kindOps(k), c.Op) {
		return false
	}
	switch k {
	case kindBool:
		want, ok := c.Value.(bool)
		return ok && (boolField(c.Field, e) == want) == (c.Op == OpEq)
	case kindNum, kindID:
		got := numField(c.Field, e, now)
		if c.Op == OpIn {
			wants, ok := nums(c.Value)
			return ok && slices.Contains(wants, got)
		}
		want, ok := num(c.Value)
		if !ok {
			return false
		}
		switch c.Op {
		case OpEq:
			return got == want
		case OpNe:
			return got != want
		case OpGt:
			return got > want
		default:
			return got < want
		}
	default:
		return c.matchStr(strField(c.Field, e))
	}
}

// matchStr applies a string operator; a list field matches when any of its
// values does, and the negative operators require that none does.
func (c Cond) matchStr(got []string) bool {
	switch c.Op {
	case OpExists:
		want, ok := c.Value.(bool)
		if c.Value == nil {
			want, ok = true, true
		}
		return ok && (len(got) > 0) == want
	case OpMatches:
		re := c.re
		if re == nil { // built in code rather than decoded
			var err error
			if re, err = compileRegex(c.Value); err != nil {
				return false
			}
		}
		return slices.ContainsFunc(got, re.MatchString)
	}
	want, ok := strs(c.Value)
	if !ok {
		return false
	}
	test := strings.Contains
	if c.Op == OpEq || c.Op == OpNe || c.Op == OpIn {
		test = func(g, w string) bool { return g == w }
		if c.Field == "from_domain" {
			test = domainMatches
		}
	}
	hit := slices.ContainsFunc(got, func(g string) bool {
		return slices.ContainsFunc(want, func(w string) bool { return test(g, strings.ToLower(w)) })
	})
	return hit == (c.Op != OpNe && c.Op != OpNotContains)
}

// domainMatches reports whether got is want or one of its subdomains.
func domainMatches(got, want string) bool {
	return got == want || strings.HasSuffix(got, "."+want)
}

// compileRegex builds the case-insensitive RE2 pattern of a "matches" leaf.
func compileRegex(v any) (*regexp.Regexp, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil, errors.New("needs a pattern")
	}
	if utf8.RuneCountInString(s) > maxRegexLen {
		return nil, fmt.Errorf("pattern is longer than %d characters", maxRegexLen)
	}
	re, err := regexp.Compile("(?i)" + s)
	if err != nil {
		return nil, fmt.Errorf("pattern does not compile: %w", err)
	}
	return re, nil
}

// list returns v as a slice when it is one, in either decoded or literal form.
func list(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out, true
	}
	return nil, false
}

// strs accepts one non-empty string or a non-empty list of them.
func strs(v any) ([]string, bool) {
	l, ok := list(v)
	if !ok {
		l = []any{v}
	}
	out := make([]string, 0, len(l))
	for _, item := range l {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, false
		}
		out = append(out, s)
	}
	return out, len(out) > 0
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// nums accepts a non-empty list of numbers.
func nums(v any) ([]float64, bool) {
	l, _ := list(v)
	out := make([]float64, 0, len(l))
	for _, item := range l {
		n, ok := num(item)
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

// validate checks the tree and returns a *ValidationError whose path points
// at the offending key, e.g. "conditions.all[0].op".
func (c Cond) validate(path string) error {
	leaf := c.Field != "" || c.Op != "" || c.Value != nil
	parts := 0
	for _, set := range []bool{leaf, len(c.All) > 0, len(c.Any) > 0} {
		if set {
			parts++
		}
	}
	if parts > 1 {
		return &ValidationError{path, "use exactly one of all, any, or field/op/value"}
	}
	for i, ch := range c.All {
		if err := ch.validate(fmt.Sprintf("%s.all[%d]", path, i)); err != nil {
			return err
		}
	}
	for i, ch := range c.Any {
		if err := ch.validate(fmt.Sprintf("%s.any[%d]", path, i)); err != nil {
			return err
		}
	}
	if !leaf {
		return nil
	}

	k, ok := fieldKind(c.Field)
	if !ok {
		return &ValidationError{path + ".field", fmt.Sprintf("unknown field %q", c.Field)}
	}
	if !slices.Contains(kindOps(k), c.Op) {
		msg := fmt.Sprintf("unknown operator %q", c.Op)
		if slices.Contains(kindOps(kindStr), c.Op) || slices.Contains(kindOps(kindNum), c.Op) {
			msg = fmt.Sprintf("operator %q does not apply to field %q", c.Op, c.Field)
		}
		return &ValidationError{path + ".op", msg}
	}
	if msg := c.valueProblem(k); msg != "" {
		return &ValidationError{path + ".value", msg}
	}
	return nil
}

// valueProblem says what is wrong with a leaf's value, or "" when it fits.
func (c Cond) valueProblem(k kind) string {
	switch {
	case c.Op == OpExists:
		if _, ok := c.Value.(bool); !ok && c.Value != nil {
			return "exists takes true or false"
		}
	case c.Op == OpMatches:
		if _, err := compileRegex(c.Value); err != nil {
			return err.Error()
		}
	case k == kindBool:
		if _, ok := c.Value.(bool); !ok {
			return "needs true or false"
		}
	case k == kindNum || k == kindID:
		if c.Op == OpIn {
			if _, ok := nums(c.Value); !ok {
				return "needs a list of numbers"
			}
		} else if _, ok := num(c.Value); !ok {
			return "needs a number"
		}
	default:
		vs, ok := strs(c.Value)
		if !ok {
			return "needs a non-empty string or list of strings"
		}
		if k == kindEnum && slices.ContainsFunc(vs, func(v string) bool { return !slices.Contains(dmarcValues, strings.ToLower(v)) }) {
			return "dmarc is one of pass, fail, none"
		}
	}
	return ""
}

// opWords is how each operator reads in Text.
var opWords = map[string]string{
	OpEq: "is", OpNe: "is not", OpIn: "is one of", OpContains: "contains", OpContainsAny: "contains any of",
	OpNotContains: "contains none of", OpMatches: "matches", OpGt: "is more than", OpLt: "is less than",
}

// Text renders the tree as one line of plain English, for showing a rule's "unless" part
// to a model: `from_domain is one of a.com, b.com and (subject contains refund or
// is_bulk is true)`. The empty tree renders as "".
func (c Cond) Text() string {
	join := func(children []Cond, sep string) string {
		var parts []string
		for _, ch := range children {
			t := ch.Text()
			if t == "" {
				continue
			}
			if len(ch.All)+len(ch.Any) > 1 {
				t = "(" + t + ")"
			}
			parts = append(parts, t)
		}
		return strings.Join(parts, sep)
	}
	switch {
	case c.Op == OpExists:
		if c.Value == false {
			return c.Field + " is missing"
		}
		return c.Field + " is present"
	case c.Field != "" || c.Op != "":
		var vals []string
		switch v := c.Value.(type) {
		case []any:
			for _, x := range v {
				vals = append(vals, fmt.Sprint(x))
			}
		case []string:
			vals = v
		default:
			vals = []string{fmt.Sprint(v)}
		}
		return c.Field + " " + cmp.Or(opWords[c.Op], c.Op) + " " + strings.Join(vals, ", ")
	case len(c.Any) > 0:
		return join(c.Any, " or ")
	default:
		return join(c.All, " and ")
	}
}
