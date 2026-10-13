package api

import (
	"net/http"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// drafter reports whether a module of the build writes draft replies.
func (s *server) drafter() bool {
	for _, m := range s.Modules {
		if m.Drafts != nil {
			return true
		}
	}
	return false
}

// cleanDraft is a rule's draft setting as it is stored: its instruction trimmed.
func cleanDraft(d *rules.Draft) *rules.Draft {
	if d == nil {
		return nil
	}
	out := *d
	out.Instruction = strings.TrimSpace(out.Instruction)
	return &out
}

// draftProblem checks a rule's draft setting, set to d from was: switching drafting off,
// and keeping it as it is, are always fine; setting it needs a build with a module that
// writes draft replies and a tenant whose plan includes them. It answers 400 invalid_input at
// path (or 403 plan_required) itself and returns false when d will not do.
func (s *server) draftProblem(w http.ResponseWriter, r *http.Request, path string, was, d *rules.Draft) bool {
	if d == nil || was != nil && *was == *d {
		return true
	}
	if !s.drafter() {
		invalid(w, path, "This build of MailRules does not draft replies.")
		return false
	}
	if ok, err := hasFeature(r.Context(), s.store, s.billing(), viewer(r).TenantID, "draft_replies", s.now()); err != nil || !ok {
		if err != nil {
			internalError(w, r, err)
		} else {
			s.planRequired(w, "draft_replies")
		}
		return false
	}
	return true
}
