package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ImportProblem is something in a rules file that this install cannot take as written. Path
// names where it is (defaults.decision_model, rules[2].applies_to); Message is a sentence
// for a person, complete without the path.
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
// address of the mailbox it applies to (not the account's number, which means nothing on
// another install), and the models in force as the defaults the rules were written against.
// min_confidence is not a default here: a rule without its own threshold takes the setting
// at the time, so the file writes none and an import does not pin the current one onto it.
func (s *Settings) ExportRules(ctx context.Context, userID int64) (rules.File, error) {
	rs, err := s.Store.Rules(ctx, userID)
	if err != nil {
		return rules.File{}, err
	}
	cfg, err := s.models(ctx)
	if err != nil {
		return rules.File{}, err
	}
	f := rules.File{Rules: rs, Mailboxes: map[string]string{},
		Defaults: rules.Defaults{DecisionModel: cfg.DeciderSpec(), FallbackModel: cfg.FallbackModel}}
	var accounts map[int64]store.Account
	for _, r := range rs {
		if r.AccountID == 0 {
			continue
		}
		if accounts == nil {
			if accounts, err = s.accountsByID(ctx); err != nil {
				return rules.File{}, err
			}
		}
		a, ok := accounts[r.AccountID]
		if !ok || a.Username == "" {
			return rules.File{}, fmt.Errorf("rule %q applies to mailbox %d, which no longer exists", r.Name, r.AccountID)
		}
		f.Mailboxes[r.Name] = a.Username
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

// PrepareImport checks a parsed rules file against this install and returns the rules to
// store, each with the mailbox it applies to. Nothing is stored here. A problem comes back
// as one *ImportProblem per fault, joined (errors.Join), and then nothing may be imported:
//
//   - The defaults' models are not applied (they are settings of the daemon, and a shared
//     file must not change which paid model an install uses), so a file written for another
//     model is refused with how to go on; one that names the models in force is fine.
//   - applies_to names a mailbox by its address, matched ignoring case. One that is not
//     connected here is refused, since importing would make the rule act on every mailbox
//     or none. "all" widens the rule to every mailbox. A rule the file says nothing about
//     keeps the mailbox it has, or has none if it is new.
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

	out := make([]rules.Rule, len(f.Rules))
	copy(out, f.Rules)
	var existing map[string]rules.Rule
	var accounts []store.Account
	for i := range out {
		r := &out[i]
		to, said := f.Mailboxes[r.Name]
		switch {
		case !said || to == "":
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
			if accounts == nil {
				if accounts, err = s.Store.Accounts(ctx); err != nil {
					return nil, err
				}
			}
			var match []store.Account
			for _, a := range accounts {
				if strings.EqualFold(a.Username, to) {
					match = append(match, a)
				}
			}
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
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return out, nil
}
