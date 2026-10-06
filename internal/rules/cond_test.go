package rules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/message"
)

var testNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// testEmail is a food-delivery receipt with one PDF attached, ten days old.
func testEmail() message.Summary {
	return message.Summary{
		AccountID:      7,
		From:           "noreply@mail.zomato.com",
		FromName:       "Zomato",
		FromDomain:     "mail.zomato.com",
		To:             []string{"me@icloud.com", "other@example.com"},
		Cc:             []string{"flatmate@example.com"},
		DeliveredTo:    []string{"shopping@icloud.com"},
		Subject:        "Your Invoice for order #4821",
		Body:           "Thanks for ordering. Total paid: Rs 412.",
		Headers:        map[string][]string{"List-Id": {"<orders.zomato.com>"}, "X-Mailer": {"Mailgun"}},
		ListID:         "orders.zomato.com",
		HasAttachment:  true,
		AttachmentExts: []string{"pdf"},
		SizeKB:         84.5,
		ReceivedAt:     testNow.Add(-10 * 24 * time.Hour),
		IsBulk:         true,
		IsNoreply:      true,
		DMARC:          "pass",
	}
}

func leaf(field, op string, value any) Cond { return Cond{Field: field, Op: op, Value: value} }

func TestMatch(t *testing.T) {
	long := strings.Repeat("a", maxRegexLen+1)
	tests := []struct {
		name string
		cond Cond
		want bool
	}{
		// Trees.
		{"empty tree is true", Cond{}, true},
		{"empty all is true", Cond{All: []Cond{}}, true},
		{"all, every child passes", Cond{All: []Cond{leaf("is_bulk", OpEq, true), leaf("dmarc", OpEq, "pass")}}, true},
		{"all, one child fails", Cond{All: []Cond{leaf("is_bulk", OpEq, true), leaf("dmarc", OpEq, "fail")}}, false},
		{"any, one child passes", Cond{Any: []Cond{leaf("is_bulk", OpEq, false), leaf("dmarc", OpEq, "pass")}}, true},
		{"any, no child passes", Cond{Any: []Cond{leaf("is_bulk", OpEq, false), leaf("dmarc", OpEq, "fail")}}, false},
		{"any nested in all", Cond{All: []Cond{
			leaf("from_domain", OpIn, []any{"swiggy.in", "zomato.com"}),
			{Any: []Cond{leaf("has_attachment", OpEq, false), leaf("subject", OpContainsAny, []any{"invoice", "receipt"})}},
		}}, true},
		{"all nested in any, failing", Cond{Any: []Cond{
			{All: []Cond{leaf("is_bulk", OpEq, true), leaf("size_kb", OpGt, 500.0)}},
			leaf("subject", OpContains, "refund"),
		}}, false},

		// Address lists.
		{"from eq", leaf("from", OpEq, "noreply@mail.zomato.com"), true},
		{"from eq ignores case", leaf("from", OpEq, "NoReply@Mail.Zomato.com"), true},
		{"from ne", leaf("from", OpNe, "boss@work.com"), true},
		{"to eq any recipient", leaf("to", OpEq, "other@example.com"), true},
		{"to ne fails when one recipient equals", leaf("to", OpNe, "other@example.com"), false},
		{"to in", leaf("to", OpIn, []any{"x@y.z", "me@icloud.com"}), true},
		{"cc contains", leaf("cc", OpContains, "flatmate"), true},
		{"delivered_to catches the alias", leaf("delivered_to", OpEq, "shopping@icloud.com"), true},
		{"to exists", leaf("to", OpExists, true), true},

		// from_domain.
		{"from_domain eq subdomain", leaf("from_domain", OpEq, "zomato.com"), true},
		{"from_domain in subdomain, mixed case", leaf("from_domain", OpIn, []any{"swiggy.in", "Zomato.COM"}), true},
		{"from_domain is not a suffix match", leaf("from_domain", OpEq, "omato.com"), false},
		{"from_domain parent does not match child rule", leaf("from_domain", OpEq, "eu.mail.zomato.com"), false},
		{"from_domain ne subdomain", leaf("from_domain", OpNe, "zomato.com"), false},
		{"from_domain ne other", leaf("from_domain", OpNe, "swiggy.in"), true},

		// Strings.
		{"subject contains ignores case", leaf("subject", OpContains, "INVOICE"), true},
		{"subject contains miss", leaf("subject", OpContains, "refund"), false},
		{"subject contains_any hit", leaf("subject", OpContainsAny, []any{"receipt", "invoice"}), true},
		{"subject contains_any miss", leaf("subject", OpContainsAny, []any{"receipt", "refund"}), false},
		{"subject not_contains none present", leaf("subject", OpNotContains, []any{"refund", "otp"}), true},
		{"subject not_contains one present", leaf("subject", OpNotContains, []any{"refund", "Invoice"}), false},
		{"subject not_contains single string", leaf("subject", OpNotContains, "order"), false},
		{"subject eq is whole-string", leaf("subject", OpEq, "invoice"), false},
		{"subject in, literal []string", leaf("subject", OpIn, []string{"your invoice for order #4821"}), true},
		{"body contains", leaf("body", OpContains, "total paid"), true},
		{"subject matches", leaf("subject", OpMatches, `order #\d+$`), true},
		{"subject matches ignores case", leaf("subject", OpMatches, `^YOUR INVOICE`), true},
		{"subject matches miss", leaf("subject", OpMatches, `^invoice`), false},
		{"matches rejects a 201-character pattern", leaf("subject", OpMatches, long), false},
		{"matches rejects a bad pattern", leaf("subject", OpMatches, `(`), false},
		{"list_id eq", leaf("list_id", OpEq, "orders.zomato.com"), true},
		{"list_id exists", leaf("list_id", OpExists, nil), true},

		// Headers.
		{"header contains, any name case", leaf("header:list-id", OpContains, "zomato"), true},
		{"header exists", leaf("header:X-Mailer", OpExists, true), true},
		{"header missing exists", leaf("header:X-Spam", OpExists, true), false},
		{"header missing, exists false", leaf("header:X-Spam", OpExists, false), true},
		{"header missing ne", leaf("header:X-Spam", OpNe, "yes"), true},

		// Attachments.
		{"has_attachment eq", leaf("has_attachment", OpEq, true), true},
		{"has_attachment ne", leaf("has_attachment", OpNe, true), false},
		{"attachment_ext in", leaf("attachment_ext", OpIn, []any{"PDF", "docx"}), true},
		{"attachment_ext eq miss", leaf("attachment_ext", OpEq, "zip"), false},

		// Numbers.
		{"size_kb gt", leaf("size_kb", OpGt, 50.0), true},
		{"size_kb lt", leaf("size_kb", OpLt, 50.0), false},
		{"size_kb eq", leaf("size_kb", OpEq, 84.5), true},
		{"size_kb ne", leaf("size_kb", OpNe, 84.5), false},
		{"age_days gt, literal int", leaf("age_days", OpGt, 7), true},
		{"age_days lt", leaf("age_days", OpLt, 7.0), false},

		// Signals.
		{"is_contact eq false", leaf("is_contact", OpEq, false), true},
		{"replied_before eq true", leaf("replied_before", OpEq, true), false},
		{"is_bulk eq", leaf("is_bulk", OpEq, true), true},
		{"is_noreply ne false", leaf("is_noreply", OpNe, false), true},
		{"dmarc eq ignores case", leaf("dmarc", OpEq, "PASS"), true},
		{"dmarc in", leaf("dmarc", OpIn, []any{"fail", "none"}), false},
		{"dmarc ne", leaf("dmarc", OpNe, "fail"), true},

		// Account.
		{"account eq", leaf("account", OpEq, 7.0), true},
		{"account ne", leaf("account", OpNe, 7.0), false},
		{"account in", leaf("account", OpIn, []any{3.0, 7.0}), true},

		// Leaves that would not validate never match, even negated.
		{"unknown field", leaf("sender", OpEq, "x"), false},
		{"unknown op", leaf("subject", "startswith", "your"), false},
		{"op of another kind", leaf("subject", OpGt, 3.0), false},
		{"wrong value type for bool", leaf("is_bulk", OpEq, "yes"), false},
		{"wrong value type for number", leaf("size_kb", OpGt, "big"), false},
		{"wrong value type for string ne", leaf("subject", OpNe, 5.0), false},
		{"wrong value type for exists", leaf("subject", OpExists, "yes"), false},
		{"empty list for in", leaf("account", OpIn, []any{}), false},
	}
	e := testEmail()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cond.Match(&e, testNow); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
			// A tree that survives the trip through storage must match the same way.
			b, err := json.Marshal(tt.cond)
			if err != nil {
				t.Fatal(err)
			}
			var stored Cond
			if err := json.Unmarshal(b, &stored); err != nil {
				t.Fatal(err)
			}
			if got := stored.Match(&e, testNow); got != tt.want {
				t.Errorf("after JSON round-trip (%s) Match = %v, want %v", b, got, tt.want)
			}
		})
	}
}

func TestCondJSON(t *testing.T) {
	var c Cond
	if err := json.Unmarshal([]byte(`{"all":[{"field":"subject","op":"matches","value":"^re:"}]}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.All[0].re == nil {
		t.Error("regex not compiled on decode")
	}
	// A misspelt key must be an error, not an always-true tree.
	if err := json.Unmarshal([]byte(`{"all":[{"feild":"subject","op":"eq","value":"x"}]}`), &c); err == nil {
		t.Error("unknown key accepted")
	}
	if b, _ := json.Marshal(Cond{}); string(b) != "{}" {
		t.Errorf("empty tree = %s, want {}", b)
	}
}

func TestCondText(t *testing.T) {
	tests := []struct{ json, want string }{
		{`{}`, ""},
		{`{"field":"from_domain","op":"in","value":["a.com","b.com"]}`, "from_domain is one of a.com, b.com"},
		{`{"all":[{"field":"subject","op":"contains","value":"refund"},{"any":[{"field":"is_bulk","op":"eq","value":true},{"field":"size_kb","op":"gt","value":10}]}]}`,
			"subject contains refund and (is_bulk is true or size_kb is more than 10)"},
		{`{"any":[{"field":"header:List-Id","op":"exists","value":false},{"field":"list_id","op":"exists","value":true}]}`,
			"header:List-Id is missing or list_id is present"},
	}
	for _, tt := range tests {
		var c Cond
		if err := json.Unmarshal([]byte(tt.json), &c); err != nil {
			t.Fatal(err)
		}
		if got := c.Text(); got != tt.want {
			t.Errorf("Text(%s) = %q, want %q", tt.json, got, tt.want)
		}
	}
}
