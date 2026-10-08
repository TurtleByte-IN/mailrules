package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ImportProblem is something in a rules file that this install cannot take as written. Path
// names where it is (defaults.decision_model, rules[2].applies_to, rules[0].match); Message
// is a sentence for a person, complete without the path.
type ImportProblem struct{ Path, Message string }

func (e *ImportProblem) Error() string { return e.Path + ": " + e.Message }

// models are the models in force, for comparing with a rules file. The provider keys are not
// read, so this works where there is no master key (the command line).
func (s *Settings) models(ctx context.Context) (config.Config, error) {
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		return config.Config{}, err
	}
	for k := range rows {
		if strings.HasPrefix(k, keyPrefix) {
			delete(rows, k)
		}
	}
	cfg, _, err := s.effective(rows)
	return cfg, err
}

// ExportRules is the user's rules as a rules file: each rule in priority order, with the
// address of the mailbox it applies to and of every mailbox its account conditions name (not
// the account's number, which means nothing on another install), and the models in force as
// the defaults the rules were written against. min_confidence is not a default here: a rule
// without its own threshold takes the setting at the time, so the file writes none and an
// import does not pin the current one onto it. A rule whose mailbox was removed has none and
// is written marked (rules.Rule.MailboxRemoved). Removing a mailbox takes it out of every
// rule (store.DeleteAccount, ReconcileRemovedMailboxes at start), so a rule naming a mailbox
// that no longer exists is a broken invariant, and stops the export rather than write a file
// that names a number.
func (s *Settings) ExportRules(ctx context.Context, userID int64) (rules.File, error) {
	rs, err := s.Store.Rules(ctx, userID)
	if err != nil {
		return rules.File{}, err
	}
	cfg, err := s.models(ctx)
	if err != nil {
		return rules.File{}, err
	}
	accounts, err := s.accountsByID(ctx)
	if err != nil {
		return rules.File{}, err
	}
	f := rules.File{Rules: rs, Mailboxes: map[string]string{},
		Defaults: rules.Defaults{DecisionModel: cfg.DeciderSpec(), FallbackModel: cfg.FallbackModel}}
	for i := range rs {
		r := &rs[i]
		if r.AccountID != 0 {
			a, ok := accounts[r.AccountID]
			if !ok || a.Username == "" {
				return rules.File{}, fmt.Errorf("rule %q applies to mailbox %d, which no longer exists", r.Name, r.AccountID)
			}
			f.Mailboxes[r.Name] = a.Username
		}
		var gone any
		address := func(v any) any {
			if id, ok := v.(float64); ok && id == math.Trunc(id) {
				if a, ok := accounts[int64(id)]; ok && a.Username != "" {
					return a.Username
				}
			}
			if gone == nil {
				gone = v
			}
			return v
		}
		r.Conditions, r.Exceptions = r.Conditions.MapAccounts(address), r.Exceptions.MapAccounts(address)
		if gone != nil {
			return rules.File{}, fmt.Errorf("rule %q has a condition on mailbox %v, which no longer exists", r.Name, gone)
		}
	}
	return f, nil
}

func (s *Settings) accountsByID(ctx context.Context) (map[int64]store.Account, error) {
	as, err := s.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]store.Account, len(as))
	for _, a := range as {
		out[a.ID] = a
	}
	return out, nil
}

// connected is the connected mailboxes whose address is addr, ignoring case.
func connected(accounts []store.Account, addr string) []store.Account {
	var out []store.Account
	for _, a := range accounts {
		if strings.EqualFold(a.Username, addr) {
			out = append(out, a)
		}
	}
	return out
}

// PrepareImport checks a parsed rules file against this install and returns the rules to
// store, each with the mailbox it applies to and the mailbox ids its account conditions
// name. Nothing is stored here. A problem comes back as one *ImportProblem per fault, joined
// (errors.Join), and then nothing may be imported:
//
//   - The defaults' models are not applied (they are settings of the daemon, and a shared
//     file must not change which paid model an install uses), so a file written for another
//     model is refused with how to go on; one that names the models in force is fine.
//   - applies_to names a mailbox by its address, matched ignoring case. One that is not
//     connected here, or that more than one connected mailbox has, is refused, since
//     importing would make the rule act on every mailbox or none. "all" widens the rule to
//     every mailbox. A rule the file says nothing about keeps the mailbox it has, or has
//     none if it is new.
//   - An account condition, in match or unless at any depth, names mailboxes the same way
//     and is refused the same way, as is one that names a mailbox by number.
//   - A rule marked mailbox_removed is stored off, marked and with no mailbox, whatever
//     enabled says; one that also has applies_to contradicts itself and is refused. A
//     marked rule here that the file names unmarked, with the same match and unless and no
//     applies_to, keeps its mark: the file chose neither a mailbox nor new conditions.
func (s *Settings) PrepareImport(ctx context.Context, userID int64, f rules.File) ([]rules.Rule, error) {
	cfg, err := s.models(ctx)
	if err != nil {
		return nil, err
	}
	var problems []error
	if m := f.Defaults.DecisionModel; m != "" && m != cfg.DeciderSpec() && m != cfg.Decider {
		problems = append(problems, &ImportProblem{"defaults.decision_model", fmt.Sprintf(
			"This file was written for the decision model %q, but this install uses %q. An import does not change the models: they are settings of this install. Change the model in Settings, or delete decision_model from the file's defaults, then import again.", m, cfg.DeciderSpec())})
	}
	if m := f.Defaults.FallbackModel; m != "" && m != cfg.FallbackModel {
		now := fmt.Sprintf("%q", cfg.FallbackModel)
		if cfg.FallbackModel == "" {
			now = "none (the fallback is off)"
		}
		problems = append(problems, &ImportProblem{"defaults.fallback_model", fmt.Sprintf(
			"This file was written for the fallback model %q, but this install uses %s. An import does not change the models: they are settings of this install. Change the model in Settings, or delete fallback_model from the file's defaults, then import again.", m, now)})
	}

	accounts, err := s.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]rules.Rule, len(f.Rules))
	copy(out, f.Rules)
	var existing map[string]rules.Rule
	for i := range out {
		r := &out[i]
		to, said := f.Mailboxes[r.Name]
		said = said && to != ""
		if r.MailboxRemoved && said {
			problems = append(problems, &ImportProblem{fmt.Sprintf("rules[%d].mailbox_removed", i), fmt.Sprintf(
				"Rule %d (%q): mailbox_removed says the rule's mailbox was removed, but applies_to names a mailbox for it. Delete one of the two, then import again.", i+1, r.Name)})
		}
		switch {
		case r.MailboxRemoved:
			r.AccountID = 0
		case !said:
			if existing == nil {
				old, err := s.Store.Rules(ctx, userID)
				if err != nil {
					return nil, err
				}
				existing = make(map[string]rules.Rule, len(old))
				for _, o := range old {
					existing[o.Name] = o
				}
			}
			r.AccountID = existing[r.Name].AccountID // 0 for a new rule
		case strings.EqualFold(to, rules.AllMailboxes):
			r.AccountID = 0
		default:
			match := connected(accounts, to)
			path := fmt.Sprintf("rules[%d].applies_to", i)
			switch len(match) {
			case 1:
				r.AccountID = match[0].ID
			case 0:
				problems = append(problems, &ImportProblem{path, fmt.Sprintf(
					"Rule %d (%q): no mailbox for %s is connected here, so the rule cannot be limited to it. Add that mailbox first, or delete applies_to to leave the rule as it is (or write applies_to: all for every mailbox).", i+1, r.Name, to)})
			default:
				problems = append(problems, &ImportProblem{path, fmt.Sprintf(
					"Rule %d (%q): more than one connected mailbox is called %s, so it is not clear which the rule is for.", i+1, r.Name, to)})
			}
		}

		for _, part := range []struct {
			key  string
			cond *rules.Cond
		}{{"match", &r.Conditions}, {"unless", &r.Exceptions}} {
			path := fmt.Sprintf("rules[%d].%s", i, part.key)
			*part.cond = part.cond.MapAccounts(func(v any) any {
				addr, ok := v.(string)
				if !ok {
					problems = append(problems, &ImportProblem{path, fmt.Sprintf("Rule %d (%q): %s", i+1, r.Name, rules.AccountByAddress)})
					return v
				}
				switch match := connected(accounts, addr); len(match) {
				case 1:
					return float64(match[0].ID) // as a stored condition has it
				case 0:
					problems = append(problems, &ImportProblem{path, fmt.Sprintf(
						"Rule %d (%q): no mailbox for %s is connected here, so its account condition cannot name it. Add that mailbox first, or change the condition.", i+1, r.Name, addr)})
				default:
					problems = append(problems, &ImportProblem{path, fmt.Sprintf(
						"Rule %d (%q): more than one connected mailbox is called %s, so it is not clear which its account condition means.", i+1, r.Name, addr)})
				}
				return v
			})
		}
		if old, ok := existing[r.Name]; ok && !said && old.MailboxRemoved && !r.MailboxRemoved &&
			sameTree(old.Conditions, r.Conditions) && sameTree(old.Exceptions, r.Exceptions) {
			r.MailboxRemoved, r.Enabled = true, false
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return out, nil
}

// sameTree reports whether two condition trees are written the same.
func sameTree(a, b rules.Cond) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}
