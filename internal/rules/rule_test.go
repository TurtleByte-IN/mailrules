package rules

import (
	"errors"
	"strings"
	"testing"
)

func conf(v float64) *float64 { return &v }

func TestValidate(t *testing.T) {
	move := []Action{{Type: ActMove, Folder: "Food"}}
	food := leaf("from_domain", OpIn, []any{"swiggy.in", "zomato.com"})
	tests := []struct {
		name     string
		rule     Rule
		wantPath string // "" = valid
	}{
		{"condition-only", Rule{Name: "food", Conditions: food, Actions: move}, ""},
		{"intent-only", Rule{Name: "jobs", Intent: "Recruiter outreach", Actions: move}, ""},
		{"both, with exceptions", Rule{Name: "jobs", Intent: "Recruiter outreach", Conditions: food,
			Exceptions: leaf("replied_before", OpEq, true), Actions: move}, ""},
		{"every action type", Rule{Name: "all", Conditions: food, Actions: []Action{{Type: ActMove, Folder: "A/B"},
			{Type: ActArchive}, {Type: ActTrash}, {Type: ActJunk}, {Type: ActFlag}, {Type: ActUnflag},
			{Type: ActRead}, {Type: ActUnread}, {Type: ActKeep}}}, ""},
		{"condition-only trash needs no threshold", Rule{Name: "spam", Conditions: food, Actions: []Action{{Type: ActTrash}}}, ""},
		{"intent trash at 0.85", Rule{Name: "scams", Intent: "Fake bank alerts", Actions: []Action{{Type: ActTrash}}, MinConfidence: conf(0.85)}, ""},
		{"stacking condition-only", Rule{Name: "flag boss", Conditions: food, Actions: []Action{{Type: ActFlag}}, Stack: true}, ""},
		{"exists without a value", Rule{Name: "lists", Conditions: leaf("list_id", OpExists, nil), Actions: move}, ""},
		{"account in", Rule{Name: "work", Conditions: leaf("account", OpIn, []any{1.0, 2.0}), Actions: move}, ""},

		{"no name", Rule{Conditions: food, Actions: move}, "name"},
		{"no conditions and no intent", Rule{Name: "x", Actions: move}, "conditions"},
		{"blank intent does not count", Rule{Name: "x", Intent: "  ", Actions: move}, "conditions"},
		{"no action", Rule{Name: "x", Conditions: food}, "actions"},
		{"unknown action", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: "delete"}}}, "actions[0].type"},
		{"move without folder", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: ActRead}, {Type: ActMove}}}, "actions[1].folder"},
		{"folder with wildcard", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: ActMove, Folder: "Food*"}}}, "actions[0].folder"},
		{"folder with percent", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: ActMove, Folder: "100%"}}}, "actions[0].folder"},
		{"folder with newline", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: ActMove, Folder: "a\r\nb"}}}, "actions[0].folder"},
		{"folder too long", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: ActMove, Folder: strings.Repeat("f", 201)}}}, "actions[0].folder"},
		{"folder on a non-move", Rule{Name: "x", Conditions: food, Actions: []Action{{Type: ActTrash, Folder: "Bin"}}}, "actions[0].folder"},
		{"bad regex", Rule{Name: "x", Conditions: Cond{All: []Cond{food, leaf("subject", OpMatches, "(")}}, Actions: move}, "conditions.all[1].value"},
		{"regex too long", Rule{Name: "x", Conditions: leaf("subject", OpMatches, strings.Repeat("a", 201)), Actions: move}, "conditions.value"},
		{"intent trash below 0.85", Rule{Name: "x", Intent: "Scams", Actions: []Action{{Type: ActTrash}}, MinConfidence: conf(0.8)}, "min_confidence"},
		{"intent trash without threshold", Rule{Name: "x", Intent: "Scams", Actions: []Action{{Type: ActTrash}}}, "min_confidence"},
		{"threshold out of range", Rule{Name: "x", Conditions: food, Actions: move, MinConfidence: conf(1.5)}, "min_confidence"},
		{"stacking with intent", Rule{Name: "x", Intent: "Newsletters", Actions: move, Stack: true}, "stack"},
		{"unknown field", Rule{Name: "x", Conditions: Cond{All: []Cond{leaf("sender", OpEq, "a")}}, Actions: move}, "conditions.all[0].field"},
		{"unknown op", Rule{Name: "x", Conditions: Cond{All: []Cond{leaf("subject", "startswith", "a")}}, Actions: move}, "conditions.all[0].op"},
		{"op of another kind", Rule{Name: "x", Conditions: leaf("subject", OpGt, 3.0), Actions: move}, "conditions.op"},
		{"unknown op deep in exceptions", Rule{Name: "x", Conditions: food, Actions: move,
			Exceptions: Cond{Any: []Cond{food, {All: []Cond{food, leaf("is_bulk", OpContains, true)}}}}}, "exceptions.any[1].all[1].op"},
		{"empty header name", Rule{Name: "x", Conditions: leaf("header:", OpExists, true), Actions: move}, "conditions.field"},
		{"group and leaf in one node", Rule{Name: "x", Conditions: Cond{All: []Cond{food}, Field: "subject", Op: OpEq, Value: "a"}, Actions: move}, "conditions"},
		{"all and any in one node", Rule{Name: "x", Conditions: Cond{All: []Cond{food}, Any: []Cond{food}}, Actions: move}, "conditions"},
		{"string where bool is needed", Rule{Name: "x", Conditions: leaf("is_bulk", OpEq, "yes"), Actions: move}, "conditions.value"},
		{"string where number is needed", Rule{Name: "x", Conditions: leaf("size_kb", OpGt, "big"), Actions: move}, "conditions.value"},
		{"scalar where id list is needed", Rule{Name: "x", Conditions: leaf("account", OpIn, 3.0), Actions: move}, "conditions.value"},
		{"empty string value", Rule{Name: "x", Conditions: leaf("subject", OpContains, ""), Actions: move}, "conditions.value"},
		{"empty list value", Rule{Name: "x", Conditions: leaf("subject", OpContainsAny, []any{}), Actions: move}, "conditions.value"},
		{"bad dmarc value", Rule{Name: "x", Conditions: leaf("dmarc", OpEq, "maybe"), Actions: move}, "conditions.value"},
		{"bad exists value", Rule{Name: "x", Conditions: leaf("list_id", OpExists, "yes"), Actions: move}, "conditions.value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rule.Validate()
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want valid", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("Validate = %v, want a ValidationError at %s", err, tt.wantPath)
			}
			if ve.Path != tt.wantPath {
				t.Errorf("path = %s (%s), want %s", ve.Path, ve.Message, tt.wantPath)
			}
		})
	}
}
