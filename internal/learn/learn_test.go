package learn

import (
	"fmt"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/message"
)

func past(from, listID string, right, at int64) Past {
	p := Past{Example: Example{RightRuleID: right}, At: at}
	p.Email.From, p.Email.ListID, p.Email.Subject = from, listID, fmt.Sprint(at)
	if from != "" {
		p.Email.FromDomain = from[len(from)-len("x.example"):]
	}
	return p
}

// The fallback model is shown at most five corrections: same sender address first, then
// same domain, then same List-Id, then whatever is most recent; newest first within each.
func TestRankOrdersBySimilarityThenRecency(t *testing.T) {
	email := message.Summary{From: "news@a.example", FromDomain: "a.example", ListID: "weekly.a.example"}
	pool := []Past{
		past("other@z.example", "", 1, 90),                 // unrelated, recent
		past("news@a.example", "", 1, 10),                  // same address, old
		past("bill@a.example", "", 2, 80),                  // same domain
		past("other@z.example", "weekly.a.example", 1, 70), // same list
		past("news@a.example", "", 2, 60),                  // same address, newer
		past("other@z.example", "", 0, 50),                 // unrelated, towards "keep"
		past("hr@a.example", "", 1, 40),                    // same domain, older
		past("other@z.example", "", 9, 99),                 // towards a rule that is no candidate now
		past("other@z.example", "", 2, 30),                 // unrelated, old
	}
	for name, tc := range map[string]struct {
		email      message.Summary
		candidates []int64
		want       string // the At of each example, in order
	}{
		"address, domain, list, then recent":     {email, []int64{1, 2}, "[60 10 80 40 70]"},
		"only corrections it can answer with":    {email, []int64{2}, "[60 80 50 30]"},
		"keep (rule 0) is always an answer":      {email, nil, "[50]"},
		"nothing alike: the most recent":         {message.Summary{From: "x@q.example", FromDomain: "q.example"}, []int64{1, 2}, "[90 80 70 60 50]"},
		"an email with no sender matches nobody": {message.Summary{}, []int64{1}, "[90 70 50 40 10]"},
		"list only":                              {message.Summary{From: "x@q.example", FromDomain: "q.example", ListID: "weekly.a.example"}, []int64{1, 2}, "[70 90 80 60 50]"},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, ex := range Rank(tc.email, pool, tc.candidates) {
				got = append(got, ex.Email.Subject)
			}
			if fmt.Sprint(got) != tc.want {
				t.Errorf("order = %v, want %s", got, tc.want)
			}
		})
	}
	if len(pool) != 9 || pool[0].At != 90 {
		t.Error("Rank reordered the caller's pool")
	}
	if got := Rank(email, nil, []int64{1}); got == nil || len(got) != 0 {
		t.Errorf("no corrections = %v, want an empty list", got)
	}
}
