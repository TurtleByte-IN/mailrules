package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtures = "../../testdata/rules"

func TestRulesCmd(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("rules:\n  - {id: a, match: {sender: x}, actions: [keep]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rulesFile := filepath.Join(fixtures, "rules.yaml")
	tests := []struct {
		name    string
		args    []string
		want    []string // substrings of stdout, in order
		wantErr string
	}{
		{"validate", []string{"validate", rulesFile}, []string{"4 rules ok"}, ""},
		{"validate reports the path", []string{"validate", bad}, nil, `rule 1 ("a"): conditions.all[0].field: unknown field "sender"`},
		{"test over fixtures", []string{"test", rulesFile, "--eml", fixtures}, []string{
			"newsletter.eml", "newsletters", "move:Reading, read, flag (+ flag failed dmarc)",
			"receipt.eml", "food", "move:Food",
			"recruiter.eml", "needs a model", "choose from: recruiters; otherwise: nothing",
		}, ""},
		{"test without --eml", []string{"test", rulesFile}, nil, "no .eml files"},
		{"test with an empty directory", []string{"test", rulesFile, "--eml", t.TempDir()}, nil, "no .eml files"},
		{"missing file", []string{"validate", filepath.Join(fixtures, "nope.yaml")}, nil, "read rules"},
		{"no file", []string{"validate"}, nil, "expected validate or test"},
		{"unknown subcommand", []string{"import", rulesFile}, nil, "expected validate or test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := rulesCmd(tt.args, &out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rest := out.String()
			for _, want := range tt.want {
				_, after, ok := strings.Cut(rest, want)
				if !ok {
					t.Fatalf("output lacks %q (in order):\n%s", want, out.String())
				}
				rest = after
			}
		})
	}
}

func TestSummaryFromEML(t *testing.T) {
	e, err := summaryFromEML(filepath.Join(fixtures, "newsletter.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if e.From != "news@lists.gopher.example" || e.FromName != "The Weekly Gopher" || e.FromDomain != "lists.gopher.example" ||
		len(e.To) != 1 || e.To[0] != "me@icloud.com" || e.ListID != "weekly.lists.gopher.example" ||
		!e.IsBulk || e.IsNoreply || e.DMARC != "fail" || e.ReceivedAt.IsZero() || e.SizeKB <= 0 ||
		!strings.HasPrefix(e.Body, "This week") || e.Headers["Precedence"][0] != "bulk" {
		t.Errorf("newsletter = %+v", e)
	}

	e, err = summaryFromEML(filepath.Join(fixtures, "recruiter.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "Senior Go engineer role – Bengaluru" || e.IsBulk || e.DMARC != "none" || e.ListID != "" {
		t.Errorf("recruiter = %+v", e)
	}

	e, _ = summaryFromEML(filepath.Join(fixtures, "receipt.eml"))
	if !e.IsNoreply || e.DMARC != "pass" {
		t.Errorf("receipt = %+v", e)
	}
	if _, err := summaryFromEML(filepath.Join(fixtures, "rules.yaml")); err == nil {
		t.Error("a non-email parsed")
	}
}
