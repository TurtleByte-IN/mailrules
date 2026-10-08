package rules

import (
	"math"
	"slices"
)

// truth is what a condition tree is known to be once a mailbox is gone: always true,
// always false, or still depending on the email (open).
type truth int

const (
	open truth = iota
	alwaysTrue
	alwaysFalse
)

// accountID is the mailbox id an account condition's value names, as MapAccounts sees it.
func accountID(v any) (int64, bool) {
	n, ok := num(v)
	if !ok || n != math.Trunc(n) {
		return 0, false
	}
	return int64(n), true
}

// AccountIDs is every mailbox the rule names, by its mailbox (AccountID) or in an account
// condition of its conditions or exceptions, without repeats.
func (r Rule) AccountIDs() []int64 {
	var out []int64
	add := func(v any) any {
		if id, ok := accountID(v); ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
		return v
	}
	if r.AccountID != 0 {
		add(float64(r.AccountID))
	}
	r.Conditions.MapAccounts(add)
	r.Exceptions.MapAccounts(add)
	return out
}

// DropAccount is the rule as it is to be once mailbox id is removed, and whether that
// changes it. No mail comes from that mailbox any more, so:
//
//   - A rule for that mailbox only is kept, switched off and marked (MailboxRemoved), with
//     no mailbox, so the user can give it another one.
//   - An account condition naming it is simplified so the rule does on the other mailboxes
//     exactly what it did: "eq" it is false, "ne" it is true, "in" loses it (false when
//     nothing is left), and the constants run up through the groups.
//   - When the conditions can then never match, or the exceptions always apply, the rule
//     could never act again: it is switched off and marked instead, with the mailbox taken
//     out of its trees so the user still sees the rest of what they wrote. A condition-only
//     rule left with no condition at all is treated the same way, as a rule without
//     conditions or intent is one MailRules refuses to save.
//   - Exceptions that can no longer apply are dropped.
//
// r is not changed.
func (r Rule) DropAccount(id int64) (Rule, bool) {
	match, mt, inMatch := simplify(r.Conditions, id)
	unless, ut, inUnless := simplify(r.Exceptions, id)
	scoped := r.AccountID == id
	if !scoped && !inMatch && !inUnless {
		return r, false
	}
	if scoped || mt == alwaysFalse || ut == alwaysTrue || (mt == alwaysTrue && r.Intent == "") {
		r.AccountID, r.Enabled, r.MailboxRemoved = 0, false, true
		r.Conditions, _ = strip(r.Conditions, id)
		r.Exceptions, _ = strip(r.Exceptions, id)
		return r, true
	}
	if inMatch {
		r.Conditions = match
		if mt == alwaysTrue {
			r.Conditions = Cond{}
		}
	}
	if inUnless {
		r.Exceptions = unless
		if ut == alwaysFalse {
			r.Exceptions = Cond{}
		}
	}
	return r, true
}

// names reports whether an account leaf's value names mailbox id, and for an "in" list
// the other items.
func names(c Cond, id int64) (hit bool, rest []any) {
	is := func(v any) bool { n, ok := accountID(v); return ok && n == id }
	if l, ok := list(c.Value); ok {
		rest = slices.DeleteFunc(slices.Clone(l), is)
		return len(rest) < len(l), rest
	}
	return is(c.Value), nil
}

// simplify is the tree once mailbox id is gone, what it is then known to be, and whether
// it named the mailbox at all. When it did not, the tree comes back unchanged and open. A
// tree known to be true or false comes back as the parts it still has; the caller puts the
// constant in its place.
func simplify(c Cond, id int64) (Cond, truth, bool) {
	if c.Field == "account" {
		hit, rest := names(c, id)
		switch {
		case !hit:
			return c, open, false
		case c.Op == OpNe:
			return c, alwaysTrue, true
		case c.Op == OpIn && len(rest) > 0:
			c.Value = rest
			return c, open, true
		default: // eq it, or in a list of only it
			return c, alwaysFalse, true
		}
	}
	if c.Field != "" || (len(c.All) == 0 && len(c.Any) == 0) {
		return c, open, false
	}
	isAny := len(c.Any) > 0
	children := c.All
	absorb, neutral := alwaysFalse, alwaysTrue // all: false wins, true drops away
	if isAny {
		children, absorb, neutral = c.Any, alwaysTrue, alwaysFalse
	}
	touched, absorbed := false, false
	kept := make([]Cond, 0, len(children))
	for _, ch := range children {
		s, t, hit := simplify(ch, id)
		touched = touched || hit
		switch t {
		case absorb:
			absorbed = true
		case neutral:
		default:
			kept = append(kept, s)
		}
	}
	if !touched {
		return c, open, false
	}
	switch {
	case absorbed:
		return c, absorb, true
	case len(kept) == 0:
		return c, neutral, true
	case isAny:
		return Cond{Any: kept}, open, true
	default:
		return Cond{All: kept}, open, true
	}
}

// strip is the tree with mailbox id taken out as written, not as logic would have it:
// leaves naming only it are dropped, it is dropped from "in" lists, and groups left empty
// are dropped. kept is false when nothing is left, and the tree is then empty.
func strip(c Cond, id int64) (out Cond, kept bool) {
	if c.Field == "account" {
		hit, rest := names(c, id)
		switch {
		case !hit:
			return c, true
		case c.Op == OpIn && len(rest) > 0:
			c.Value = rest
			return c, true
		default:
			return Cond{}, false
		}
	}
	if c.Field != "" || c.IsEmpty() {
		return c, !c.IsEmpty()
	}
	group := func(cs []Cond) []Cond {
		out := make([]Cond, 0, len(cs))
		for _, ch := range cs {
			if s, ok := strip(ch, id); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if len(c.Any) > 0 {
		if l := group(c.Any); len(l) > 0 {
			return Cond{Any: l}, true
		}
		return Cond{}, false
	}
	if l := group(c.All); len(l) > 0 {
		return Cond{All: l}, true
	}
	return Cond{}, false
}
