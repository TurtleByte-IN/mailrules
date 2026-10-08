// Package rules holds the rule types, the condition matcher, validation, the
// evaluation order for one email and YAML import/export. It never calls a
// model and never touches a mailbox.
package rules

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Action types, in the order the plan lists them.
const (
	ActMove    = "move"
	ActArchive = "archive"
	ActTrash   = "trash"
	ActJunk    = "junk"
	ActFlag    = "flag"
	ActUnflag  = "unflag"
	ActRead    = "read"
	ActUnread  = "unread"
	ActKeep    = "keep"
)

var actionTypes = []string{ActMove, ActArchive, ActTrash, ActJunk, ActFlag, ActUnflag, ActRead, ActUnread, ActKeep}

// MinTrashConfidence is the lowest threshold an intent-based trash rule may have.
const MinTrashConfidence = 0.85

// maxFolderLen caps a folder name, in characters.
const maxFolderLen = 200

// Action is one step a rule applies to an email. Folder is set for move only.
type Action struct {
	Type   string `json:"type"`
	Folder string `json:"folder,omitempty"`
}

// String is the YAML shorthand: "move:Food", "trash".
func (a Action) String() string {
	if a.Folder != "" {
		return a.Type + ":" + a.Folder
	}
	return a.Type
}

// Rule mirrors one row of the rules table.
type Rule struct {
	ID            int64
	UserID        int64
	AccountID     int64 // 0 = all accounts
	Name          string
	Said          string // the user's original wording; "" when they gave none
	Template      string // the name of the gallery template it was added from; "" = none
	Intent        string // plain-English intent for the decider; "" = condition-only
	Conditions    Cond
	Exceptions    Cond // "unless"
	Actions       []Action
	Priority      int // lower runs first
	Stack         bool
	Model         string   // per-rule decider override
	MinConfidence *float64 // nil = the configured default
	Enabled       bool
	Version       int
	CreatedAt     int64
	UpdatedAt     int64
}

// ValidationError is a rule problem. Path is the offending key as the API reports it,
// e.g. "conditions.all[0].op". Message is a sentence for a person, complete without the
// path: capitalised, ending with a full stop, and naming no field path. Error joins the
// two for a log or the command line.
type ValidationError struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string { return e.Path + ": " + e.Message }

// Validate checks a rule before it is saved or imported and returns the first
// problem as a *ValidationError.
func (r Rule) Validate() error { return r.validate(false) }

// validate is Validate for a rule in the app (file unset) or as a rules file has it, where
// an account condition names mailboxes by address.
func (r Rule) validate(file bool) error {
	hasIntent := strings.TrimSpace(r.Intent) != ""
	if strings.TrimSpace(r.Name) == "" {
		return &ValidationError{"name", "A rule needs a name."}
	}
	if r.Conditions.IsEmpty() && !hasIntent {
		return &ValidationError{"conditions", "A rule needs conditions, an intent, or both."}
	}
	if err := r.Conditions.validate("conditions", file); err != nil {
		return err
	}
	if err := r.Exceptions.validate("exceptions", file); err != nil {
		return err
	}
	if len(r.Actions) == 0 {
		return &ValidationError{"actions", "A rule needs at least one action."}
	}
	for i, a := range r.Actions {
		path := fmt.Sprintf("actions[%d]", i)
		if !slices.Contains(actionTypes, a.Type) {
			return &ValidationError{path + ".type", fmt.Sprintf("Unknown action %q.", a.Type)}
		}
		if msg := folderProblem(a); msg != "" {
			return &ValidationError{path + ".folder", msg}
		}
	}
	if r.MinConfidence != nil && (*r.MinConfidence < 0 || *r.MinConfidence > 1) {
		return &ValidationError{"min_confidence", "The confidence threshold must be between 0 and 1."}
	}
	trashes := slices.ContainsFunc(r.Actions, func(a Action) bool { return a.Type == ActTrash })
	if hasIntent && trashes && (r.MinConfidence == nil || *r.MinConfidence < MinTrashConfidence) {
		return &ValidationError{"min_confidence", fmt.Sprintf("A rule that trashes on intent needs a confidence threshold of at least %v.", MinTrashConfidence)}
	}
	if r.Stack && hasIntent {
		return &ValidationError{"stack", "A stacking rule is condition-only: it cannot have an intent."}
	}
	return nil
}

// FolderProblem says, in a sentence, what is wrong with a folder name a rule would move
// mail to, or "" when it is fine. It is the check a move action's folder gets.
func FolderProblem(name string) string { return folderProblem(Action{Type: ActMove, Folder: name}) }

func folderProblem(a Action) string {
	if a.Type != ActMove {
		if a.Folder != "" {
			return "Only a move action takes a folder."
		}
		return ""
	}
	if n := utf8.RuneCountInString(a.Folder); n < 1 || n > maxFolderLen {
		return fmt.Sprintf("A folder name must be 1 to %d characters.", maxFolderLen)
	}
	if strings.ContainsAny(a.Folder, "*%") || strings.ContainsFunc(a.Folder, unicode.IsControl) {
		return "A folder name must not contain wildcards or control characters."
	}
	return ""
}
