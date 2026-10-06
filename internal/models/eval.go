package models

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/message"
)

// LabeledRule is one rule of an eval file's rule set.
type LabeledRule struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Intent     string `json:"intent"`
	Exceptions string `json:"exceptions"`
}

// LabeledEmail is one email summary and the rule a person says it matches.
type LabeledEmail struct {
	Email    message.Summary
	Expected int64 // 0 = none of the rules
}

// Labels is a parsed labeled.jsonl.
type Labels struct {
	Rules  []LabeledRule
	Emails []LabeledEmail
}

// labelLine is one line of labeled.jsonl: either {"rules": [...]} or
// {"email": {...}, "expected": <rule id>}.
type labelLine struct {
	Rules []LabeledRule `json:"rules"`
	Email *struct {
		From          string   `json:"from"`
		FromName      string   `json:"from_name"`
		To            []string `json:"to"`
		Subject       string   `json:"subject"`
		Body          string   `json:"body"`
		ListID        string   `json:"list_id"`
		Attachments   []string `json:"attachments"`
		IsContact     bool     `json:"is_contact"`
		RepliedBefore bool     `json:"replied_before"`
		IsBulk        bool     `json:"is_bulk"`
		DMARC         string   `json:"dmarc"`
	} `json:"email"`
	Expected *int64 `json:"expected"`
}

// LoadLabels parses labeled.jsonl and checks every expected rule exists.
func LoadLabels(r io.Reader) (*Labels, error) {
	l := &Labels{}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var line labelLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("labels line %d: %w", n, err)
		}
		switch {
		case line.Rules != nil:
			l.Rules = append(l.Rules, line.Rules...)
		case line.Email != nil && line.Expected != nil:
			e := line.Email
			_, domain, _ := strings.Cut(e.From, "@")
			l.Emails = append(l.Emails, LabeledEmail{Expected: *line.Expected, Email: message.Summary{
				From: e.From, FromName: e.FromName, FromDomain: domain, To: e.To, Subject: e.Subject, Body: e.Body,
				ListID: e.ListID, HasAttachment: len(e.Attachments) > 0, AttachmentExts: e.Attachments,
				IsContact: e.IsContact, RepliedBefore: e.RepliedBefore, IsBulk: e.IsBulk, DMARC: e.DMARC,
			}})
		default:
			return nil, fmt.Errorf("labels line %d: want \"rules\", or \"email\" with \"expected\"", n)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read labels: %w", err)
	}
	if len(l.Rules) == 0 || len(l.Emails) == 0 {
		return nil, fmt.Errorf("labels need at least one rule and one email")
	}
	for i, e := range l.Emails {
		if e.Expected != 0 && !slices.ContainsFunc(l.Rules, func(r LabeledRule) bool { return r.ID == e.Expected }) {
			return nil, fmt.Errorf("labels email %d expects rule %d, which is not in the rule set", i+1, e.Expected)
		}
	}
	return l, nil
}

// Report is the outcome of one eval run.
type Report struct {
	Decider   string
	Fallback  string // "" = no fallback
	Total     int
	Correct   int
	Errors    int // emails the decider could not answer; they count as wrong
	Escalated int
	CostUSD   float64
	Latencies []time.Duration // per answered email, both calls added, sorted
	Confusion map[[2]string]int
}

// Evaluate runs every labeled email through the router, one at a time so the
// latency numbers are not skewed by our own concurrency.
// ponytail: sequential; fan out under the Caller's cap if 500 lines get slow.
func Evaluate(ctx context.Context, r *Router, l *Labels) Report {
	rep := Report{Decider: r.Name(), Total: len(l.Emails), Confusion: map[[2]string]int{}}
	if r.Fallback != nil {
		rep.Fallback = r.Fallback.Name()
	}
	names := map[int64]string{0: "none"}
	cands := make([]Candidate, 0, len(l.Rules))
	for _, rule := range l.Rules {
		names[rule.ID] = rule.Name
		cands = append(cands, Candidate{RuleID: rule.ID, Name: rule.Name, Intent: rule.Intent, Exceptions: rule.Exceptions})
	}
	for _, e := range l.Emails {
		res, err := r.Route(ctx, DecideRequest{Email: e.Email, Candidates: cands})
		if err != nil {
			rep.Errors++
			rep.Confusion[[2]string{names[e.Expected], "error"}]++
			continue
		}
		latency, cost := res.Primary.Latency, res.Primary.CostUSD
		if res.Fallback != nil {
			latency += res.Fallback.Latency
			cost += res.Fallback.CostUSD
		}
		rep.Latencies = append(rep.Latencies, latency)
		rep.CostUSD += cost
		if res.Escalated {
			rep.Escalated++
		}
		if res.RuleID == e.Expected {
			rep.Correct++
		} else {
			rep.Confusion[[2]string{names[e.Expected], names[res.RuleID]}]++
		}
	}
	slices.Sort(rep.Latencies)
	return rep
}

// percentile is the nearest-rank percentile of sorted durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p/100+0.999999) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// Write prints the report: accuracy, share escalated, latency, cost and the
// confused rule pairs.
func (rep Report) Write(w io.Writer) {
	pct := func(n int) float64 { return 100 * float64(n) / float64(max(rep.Total, 1)) }
	fallback := rep.Fallback
	if fallback == "" {
		fallback = "off"
	}
	fmt.Fprintf(w, "decider: %s (fallback: %s)\n", rep.Decider, fallback)
	fmt.Fprintf(w, "  emails:     %d (%d errors)\n", rep.Total, rep.Errors)
	fmt.Fprintf(w, "  accuracy:   %.1f%% (%d/%d)\n", pct(rep.Correct), rep.Correct, rep.Total)
	fmt.Fprintf(w, "  escalated:  %.1f%% (%d/%d)\n", pct(rep.Escalated), rep.Escalated, rep.Total)
	fmt.Fprintf(w, "  latency:    p50 %s, p95 %s\n",
		percentile(rep.Latencies, 50).Round(time.Millisecond), percentile(rep.Latencies, 95).Round(time.Millisecond))
	fmt.Fprintf(w, "  cost:       $%.4f per 1,000 emails\n", rep.CostUSD/float64(max(len(rep.Latencies), 1))*1000)
	if len(rep.Confusion) == 0 {
		fmt.Fprintln(w, "  confusion:  none")
		return
	}
	fmt.Fprintln(w, "  confusion (expected -> got):")
	pairs := make([][2]string, 0, len(rep.Confusion))
	for p := range rep.Confusion {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if ci, cj := rep.Confusion[pairs[i]], rep.Confusion[pairs[j]]; ci != cj {
			return ci > cj
		}
		return pairs[i][0]+pairs[i][1] < pairs[j][0]+pairs[j][1]
	})
	for _, p := range pairs {
		fmt.Fprintf(w, "    %s -> %s: %d\n", p[0], p[1], rep.Confusion[p])
	}
}
