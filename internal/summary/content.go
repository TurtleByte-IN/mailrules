package summary

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/TurtleByte-IN/mailrules/internal/actions"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// The most emails a summary lists by name in each group; the rest are counted and linked.
const (
	maxTrashed = 20
	maxReview  = 10
)

// Content is what one summary reports, read from the store. It holds senders, subjects,
// rule names and counts; never an email's text.
type Content struct {
	Start, End   time.Time // the time it covers, in the user's zone
	DryRun       bool      // dry-run is on: what it lists was recorded, not done
	Rules        []store.RuleUse
	Sorted       int // emails a rule was applied to, trashed ones included: the sum of Rules
	Trashed      []store.SummaryEmail
	TrashedTotal int
	Review       []store.SummaryEmail // waiting in Needs review now
	ReviewTotal  int
	CostUSD      float64 // what the model decisions in the period cost
	ModelCalls   int
}

// build reads what MailRules did for the user in [start, end).
func build(ctx context.Context, st *store.Store, userID int64, start, end time.Time, dryRun bool) (Content, error) {
	c := Content{Start: start, End: end, DryRun: dryRun}
	from, to := start.Unix(), end.Unix()
	var err error
	if c.Rules, err = st.SummaryRules(ctx, userID, from, to); err != nil {
		return c, err
	}
	for _, r := range c.Rules {
		c.Sorted += r.Emails
	}
	if c.Trashed, c.TrashedTotal, err = st.SummaryTrashed(ctx, userID, from, to, maxTrashed); err != nil {
		return c, err
	}
	if c.Review, c.ReviewTotal, err = st.SummaryReview(ctx, userID, maxReview); err != nil {
		return c, err
	}
	if c.CostUSD, c.ModelCalls, err = st.SummaryCost(ctx, userID, from, to); err != nil {
		return c, err
	}
	return c, nil
}

// Email is a rendered summary.
type Email struct {
	Subject string
	Text    string
	HTML    string
}

// links are the places in the web app a summary points at. base is MAILRULES_PUBLIC_URL;
// the app's routes follow its #.
type links struct{ base string }

func (l links) at(route string) string { return strings.TrimRight(l.base, "/") + "/#/" + route }
func (l links) Review() string         { return l.at("review") }
func (l links) Trashed() string        { return l.at("activity?outcome=trashed") }
func (l links) Activity() string       { return l.at("activity") } // Sorted counts trashed mail too, as the Overview does
func (l links) Settings() string       { return l.at("settings") }
func (l links) Email(id int64) string  { return l.at("activity?id=" + strconv.FormatInt(id, 10)) }
func (l links) Rule(id int64) string {
	if id == 0 {
		return l.Activity()
	}
	return l.at("rules?id=" + strconv.FormatInt(id, 10))
}

// clean makes text from an email header safe to show on one line: control characters and
// line breaks become spaces, runs of spaces one, and it is cut at max runes.
func clean(s string, max int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if r := []rune(s); len(r) > max {
		return strings.TrimSpace(string(r[:max-1])) + "…"
	}
	return s
}

func sender(e store.SummaryEmail) string {
	if name := clean(e.FromName, 60); name != "" {
		return name
	}
	if addr := clean(e.FromAddr, 80); addr != "" {
		return addr
	}
	return "Unknown sender"
}

func subject(e store.SummaryEmail) string {
	if s := clean(e.Subject, 100); s != "" {
		return s
	}
	return "(no subject)"
}

// destination is where a trash took an email.
func destination(e store.SummaryEmail) string {
	switch e.Folder {
	case "":
		return "Trash"
	case actions.TrashFolder:
		return actions.TrashFolder
	}
	return clean(e.Folder, 60)
}

func money(usd float64) string {
	switch {
	case usd == 0:
		return "$0.00"
	case usd < 0.01:
		return "under $0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// title is the subject line: the counts that matter, plainly.
func (c Content) title() string {
	review := "nothing to review"
	if c.ReviewTotal > 0 {
		review = strconv.Itoa(c.ReviewTotal) + " to review"
	}
	if c.DryRun {
		return fmt.Sprintf("MailRules dry-run: %d would be sorted, %s", c.Sorted, review)
	}
	return fmt.Sprintf("MailRules: %d sorted, %s", c.Sorted, review)
}

func (c Content) span() string {
	const layout = "Mon 2 Jan 15:04"
	return c.Start.Format(layout) + " to " + c.End.Format(layout) + " (" + c.Start.Location().String() + ")"
}

func (c Content) cost() string {
	if c.ModelCalls == 0 {
		return "Model cost: none, no model was asked."
	}
	return "Model cost: " + money(c.CostUSD) + " for " + plural(c.ModelCalls, "model decision", "model decisions") + "."
}

const dryRunNote = "Dry-run is on, so MailRules changed nothing in your mailbox. This is what it would have done."

// render writes c as an email whose links start at base.
func render(c Content, base string) (Email, error) {
	l := links{base}
	var t strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&t, format+"\n", a...) }
	line("MailRules summary, %s", c.span())
	line("")
	if c.DryRun {
		line("%s", dryRunNote)
		line("")
	}
	line("Needs review (%d)", c.ReviewTotal)
	if c.ReviewTotal == 0 {
		line("Nothing is waiting for you.")
	}
	for _, e := range c.Review {
		line("- %s: %s", sender(e), subject(e))
	}
	if more := c.ReviewTotal - len(c.Review); more > 0 {
		line("- and %d more", more)
	}
	if c.ReviewTotal > 0 {
		line("Review them: %s", l.Review())
	}
	line("")
	line("Sorted (%d)", c.Sorted)
	if len(c.Rules) == 0 {
		line("No rule was applied.")
	}
	for _, r := range c.Rules {
		line("- %s: %d", clean(r.RuleName, 80), r.Emails)
	}
	if c.Sorted > 0 {
		line("See them in Activity: %s", l.Activity())
	}
	line("")
	line("Trashed (%d)", c.TrashedTotal)
	if c.TrashedTotal == 0 {
		line("Nothing was trashed.")
	}
	for _, e := range c.Trashed {
		verb := "went to"
		if e.DryRun {
			verb = "would have gone to"
		}
		line("- %s: %s, %s %s", sender(e), subject(e), verb, destination(e))
		line("  Wrong? Restore it: %s", l.Email(e.MessageID))
	}
	if more := c.TrashedTotal - len(c.Trashed); more > 0 {
		line("- and %d more", more)
	}
	if c.TrashedTotal > 0 {
		line("Everything trashed: %s", l.Trashed())
	}
	line("")
	line("%s", c.cost())
	line("")
	line("You get this email because the summary email is on in MailRules. Change or stop it in Settings: %s", l.Settings())

	var h bytes.Buffer
	if err := page.Execute(&h, struct {
		C       Content
		L       links
		Note    string
		Span    string
		Title   string
		Cost    string
		MoreRev int
		MoreTr  int
	}{c, l, dryRunNote, c.span(), c.title(), c.cost(), c.ReviewTotal - len(c.Review), c.TrashedTotal - len(c.Trashed)}); err != nil {
		return Email{}, fmt.Errorf("render summary: %w", err)
	}
	return Email{Subject: c.title(), Text: t.String(), HTML: h.String()}, nil
}

var page = template.Must(template.New("summary").Funcs(template.FuncMap{
	"sender": sender, "subject": subject, "destination": destination, "clean": func(s string) string { return clean(s, 80) },
}).Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#ffffff;color:#1c1c1c;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;">
<div style="max-width:600px;margin:0 auto;padding:24px 16px;">
<h1 style="margin:0 0 4px;font-size:20px;font-weight:600;">{{.Title}}</h1>
<p style="margin:0 0 20px;color:#5c5c5c;font-size:13px;">{{.Span}}</p>
{{if .C.DryRun}}<p style="margin:0 0 20px;padding:10px 12px;background:#fff6db;color:#5a4200;border-radius:4px;">{{.Note}}</p>{{end}}

<h2 style="margin:0 0 6px;font-size:16px;font-weight:600;">Needs review ({{.C.ReviewTotal}})</h2>
{{if eq .C.ReviewTotal 0}}<p style="margin:0 0 20px;">Nothing is waiting for you.</p>{{else}}
<ul style="margin:0 0 8px;padding-left:20px;">{{range .C.Review}}<li><strong>{{sender .}}</strong>: {{subject .}}</li>{{end}}{{if gt .MoreRev 0}}<li>and {{.MoreRev}} more</li>{{end}}</ul>
<p style="margin:0 0 20px;"><a href="{{.L.Review}}" style="color:#1a4fd6;font-weight:600;">Review them</a></p>{{end}}

<h2 style="margin:0 0 6px;font-size:16px;font-weight:600;">Sorted ({{.C.Sorted}})</h2>
{{if not .C.Rules}}<p style="margin:0 0 20px;">No rule was applied.</p>{{else}}
<ul style="margin:0 0 8px;padding-left:20px;">{{range .C.Rules}}<li><a href="{{$.L.Rule .RuleID}}" style="color:#1a4fd6;">{{clean .RuleName}}</a>: {{.Emails}}</li>{{end}}</ul>
<p style="margin:0 0 20px;"><a href="{{.L.Activity}}" style="color:#1a4fd6;">See them in Activity</a></p>{{end}}

<h2 style="margin:0 0 6px;font-size:16px;font-weight:600;">Trashed ({{.C.TrashedTotal}})</h2>
{{if eq .C.TrashedTotal 0}}<p style="margin:0 0 20px;">Nothing was trashed.</p>{{else}}
<ul style="margin:0 0 8px;padding-left:20px;">{{range .C.Trashed}}<li><strong>{{sender .}}</strong>: {{subject .}}, {{if .DryRun}}would have gone to{{else}}went to{{end}} {{destination .}}. <a href="{{$.L.Email .MessageID}}" style="color:#1a4fd6;">Wrong? Restore it</a></li>{{end}}{{if gt .MoreTr 0}}<li>and {{.MoreTr}} more</li>{{end}}</ul>
<p style="margin:0 0 20px;"><a href="{{.L.Trashed}}" style="color:#1a4fd6;">Everything trashed</a></p>{{end}}

<p style="margin:0 0 20px;color:#5c5c5c;">{{.Cost}}</p>
<p style="margin:0;padding-top:12px;border-top:1px solid #e2e2e2;color:#5c5c5c;font-size:13px;">You get this email because the summary email is on in MailRules. <a href="{{.L.Settings}}" style="color:#1a4fd6;">Change or stop it in Settings</a>.</p>
</div>
</body></html>
`))
