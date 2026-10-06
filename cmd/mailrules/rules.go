package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

const rulesUsage = `usage: mailrules rules validate <file>
       mailrules rules test <file> --eml <dir>
`

// rulesCmd runs the offline rule commands: both read a rules YAML file and
// neither touches a mailbox, a model or the database.
func rulesCmd(args []string, out io.Writer) error {
	if len(args) < 2 || (args[0] != "validate" && args[0] != "test") {
		fmt.Fprint(os.Stderr, rulesUsage)
		return errors.New("rules: expected validate or test, then a rules file")
	}
	data, err := os.ReadFile(args[1]) // #nosec G304 G703 -- the operator names the file on the command line
	if err != nil {
		return fmt.Errorf("read rules: %w", err)
	}
	f, err := rules.ParseYAML(data)
	if err != nil {
		return fmt.Errorf("%s:\n%w", args[1], err)
	}
	if args[0] == "validate" {
		_, err := fmt.Fprintf(out, "%s: %d rules ok\n", args[1], len(f.Rules))
		return err
	}

	fs := flag.NewFlagSet("rules test", flag.ContinueOnError)
	dir := fs.String("eml", "", "directory of .eml files to run the rules over")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(*dir, "*.eml"))
	if err != nil || *dir == "" || len(files) == 0 {
		return fmt.Errorf("rules test: no .eml files in --eml %q", *dir)
	}
	return testRules(f.Rules, files, out)
}

// testRules prints, per email, what the conditions alone decide. It never
// calls a model: where one would be asked, it lists the rules it would be
// asked to choose between.
func testRules(rs []rules.Rule, files []string, out io.Writer) error {
	names := map[int64]string{}
	for i := range rs {
		rs[i].ID = int64(i + 1) // a file has no database ids
		names[rs[i].ID] = rs[i].Name
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, path := range files {
		e, err := summaryFromEML(path)
		if err != nil {
			return err
		}
		ev := rules.Evaluate(e, rs, nil, rules.Options{Now: time.Now()})
		line := "no rule matches"
		switch {
		case ev.Final == nil:
			var cands []string
			for _, c := range ev.Candidates {
				cands = append(cands, c.Name)
			}
			otherwise := "nothing"
			if ev.CutOff != nil {
				otherwise = ev.CutOff.Name
			}
			line = fmt.Sprintf("needs a model\tchoose from: %s; otherwise: %s", strings.Join(cands, ", "), otherwise)
		case ev.Final.Stage != rules.StageNone:
			acts := make([]string, len(ev.Final.Actions))
			for i, a := range ev.Final.Actions {
				acts[i] = a.String()
			}
			line = fmt.Sprintf("%s\t%s", names[ev.Final.RuleID], strings.Join(acts, ", "))
			for _, id := range ev.Final.Stacked {
				line += " (+ " + names[id] + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\n", filepath.Base(path), line)
	}
	return tw.Flush()
}

// summaryFromEML reads an .eml fixture into the type the matcher runs on.
// ponytail: headers only, through net/mail. The body is taken as plain text
// (no MIME decoding, no HTML-to-text), attachments are not detected and the
// contact signals stay false. Swap this for the internal/message parser once
// that milestone lands.
func summaryFromEML(path string) (message.Summary, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixture directory named on the command line
	if err != nil {
		return message.Summary{}, fmt.Errorf("read email: %w", err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		return message.Summary{}, fmt.Errorf("parse %s: %w", path, err)
	}
	h := m.Header
	body, _ := io.ReadAll(io.LimitReader(m.Body, 2000)) // the BODY_CHARS default
	addrs := func(key string) []string {
		list, _ := h.AddressList(key)
		out := make([]string, 0, len(list))
		for _, a := range list {
			out = append(out, strings.ToLower(a.Address))
		}
		return out
	}

	e := message.Summary{
		To:          addrs("To"),
		Cc:          addrs("Cc"),
		DeliveredTo: addrs("Delivered-To"),
		Body:        string(body),
		Headers:     h,
		SizeKB:      float64(len(data)) / 1024,
		DMARC:       "none",
	}
	if from, err := mail.ParseAddress(h.Get("From")); err == nil {
		e.From, e.FromName = strings.ToLower(from.Address), from.Name
		local, domain, _ := strings.Cut(e.From, "@")
		e.FromDomain = domain
		e.IsNoreply = strings.Contains(strings.NewReplacer("-", "", "_", "", ".", "").Replace(local), "noreply")
	}
	if e.Subject, err = new(mime.WordDecoder).DecodeHeader(h.Get("Subject")); err != nil {
		e.Subject = h.Get("Subject")
	}
	e.ReceivedAt, _ = h.Date()
	if _, id, ok := strings.Cut(h.Get("List-Id"), "<"); ok {
		e.ListID = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(id), ">"))
	}
	precedence := strings.ToLower(h.Get("Precedence"))
	e.IsBulk = h.Get("List-Unsubscribe") != "" || precedence == "bulk" || precedence == "list"
	if _, v, ok := strings.Cut(strings.ToLower(h.Get("Authentication-Results")), "dmarc="); ok {
		if strings.HasPrefix(v, "pass") {
			e.DMARC = "pass"
		} else if strings.HasPrefix(v, "fail") {
			e.DMARC = "fail"
		}
	}
	return e, nil
}
