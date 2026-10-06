package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/store"
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
		{"validate reports the path", []string{"validate", bad}, nil, `rule 1 ("a"): conditions.all[0].field: Unknown field "sender".`},
		{"test over fixtures", []string{"test", rulesFile, "--eml", fixtures}, []string{
			"newsletter.eml", "newsletters", "move:Reading, read, flag (+ flag failed dmarc)",
			"receipt.eml", "food", "move:Food",
			"recruiter.eml", "needs a model", "choose from: recruiters; otherwise: nothing",
		}, ""},
		{"test without --eml", []string{"test", rulesFile}, nil, "no .eml files"},
		{"test with an empty directory", []string{"test", rulesFile, "--eml", t.TempDir()}, nil, "no .eml files"},
		{"missing file", []string{"validate", filepath.Join(fixtures, "nope.yaml")}, nil, "read rules"},
		{"no file", []string{"validate"}, nil, "expected import, export, validate or test"},
		{"unknown subcommand", []string{"frobnicate", rulesFile}, nil, "expected import, export, validate or test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := rulesCmd(t.Context(), tt.args, os.Getenv, &out)
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

// import and export work on the database, the same as the HTTP endpoints.
func TestRulesImportExport(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	getenv := func(k string) string { return map[string]string{"MAILRULES_DATA_DIR": dir}[k] }
	rulesFile := filepath.Join(fixtures, "rules.yaml")
	var out bytes.Buffer

	if err := rulesCmd(ctx, []string{"import", rulesFile}, getenv, &out); err == nil || !strings.Contains(err.Error(), "no admin user yet") {
		t.Fatalf("import before first-run setup: %v", err)
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(db).CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	for _, want := range []string{"4 rules added, 0 updated", "0 rules added, 4 updated"} {
		out.Reset()
		if err := rulesCmd(ctx, []string{"import", rulesFile}, getenv, &out); err != nil || !strings.Contains(out.String(), want) {
			t.Fatalf("import: %v, output %q, want %q", err, out.String(), want)
		}
	}
	exported := filepath.Join(dir, "out.yaml")
	if err := rulesCmd(ctx, []string{"export", exported}, getenv, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := rulesCmd(ctx, []string{"validate", exported}, getenv, &out); err != nil || !strings.Contains(out.String(), "4 rules ok") {
		t.Fatalf("the export does not validate: %v %q", err, out.String())
	}
	out.Reset()
	if err := rulesCmd(ctx, []string{"export"}, getenv, &out); err != nil || !strings.Contains(out.String(), "id: newsletters") {
		t.Fatalf("export to stdout: %v %q", err, out.String())
	}
	for _, args := range [][]string{{"import"}, {"export", "a", "b"}, {"import", filepath.Join(dir, "nope.yaml")}} {
		if err := rulesCmd(ctx, args, getenv, &out); err == nil {
			t.Errorf("rules %v: no error", args)
		}
	}
}
