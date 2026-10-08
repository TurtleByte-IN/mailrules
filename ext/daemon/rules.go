package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const rulesUsage = `usage: mailrules rules import <file>
       mailrules rules export [file]
       mailrules rules validate <file>
       mailrules rules test <file> --eml <dir>
`

// rulesCmd runs the rule commands. import and export work on the database (a running
// daemon reads the rules for every email, so it follows an import at once); validate and
// test only read a rules YAML file and touch no mailbox, model or database.
func rulesCmd(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) > 0 && (args[0] == "import" || args[0] == "export") {
		return rulesDB(ctx, args, getenv, out)
	}
	if len(args) < 2 || (args[0] != "validate" && args[0] != "test") {
		fmt.Fprint(os.Stderr, rulesUsage)
		return errors.New("rules: expected import, export, validate or test")
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

// summaryFromEML reads an .eml file through the same parser as live mail.
// ponytail: attachments come from the server's BODYSTRUCTURE, which a file does not have,
// so has_attachment and attachment_ext never match here; the contact signals stay false
// because there is no account.
func summaryFromEML(path string) (message.Summary, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixture directory named on the command line
	if err != nil {
		return message.Summary{}, fmt.Errorf("read email: %w", err)
	}
	// Raw.Header and Raw.Text are read back to back, so the whole file can go in one.
	e, err := message.Parse(&message.Raw{Header: data, Size: int64(len(data))}, 0, 2000) // the BODY_CHARS default
	if err != nil {
		return message.Summary{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if e.From == "" {
		return message.Summary{}, fmt.Errorf("parse %s: not an email: it has no From header", path)
	}
	if dates := e.Headers["Date"]; len(dates) > 0 {
		e.ReceivedAt, _ = mail.ParseDate(dates[0]) // a file has no server arrival time
	}
	return *e, nil
}

// rulesDB imports a rules YAML file into the database or exports the rules as one. An
// import replaces the rules whose names the file uses and adds the others after the
// existing ones, the same as POST /api/rules/import.
func rulesDB(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if (args[0] == "import" && len(args) != 2) || len(args) > 2 {
		fmt.Fprint(os.Stderr, rulesUsage)
		return fmt.Errorf("rules %s: wrong number of arguments", args[0])
	}
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return err
	}
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	st := store.New(db)
	user, err := st.FirstUser(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("rules %s: no admin user yet; run `mailrules serve` and finish first-run setup in the browser", args[0])
	}
	if err != nil {
		return err
	}

	if args[0] == "export" {
		rs, err := st.Rules(ctx, user.ID)
		if err != nil {
			return err
		}
		data, err := rules.MarshalYAML(rules.File{Rules: rs})
		if err != nil {
			return err
		}
		if len(args) == 2 {
			return os.WriteFile(args[1], data, 0o600) // #nosec G304 G703 -- the operator names the file on the command line
		}
		_, err = out.Write(data)
		return err
	}
	data, err := os.ReadFile(args[1]) // #nosec G304 G703 -- the operator names the file on the command line
	if err != nil {
		return fmt.Errorf("read rules: %w", err)
	}
	f, err := rules.ParseYAML(data)
	if err != nil {
		return fmt.Errorf("%s:\n%w", args[1], err)
	}
	created, updated, err := st.ImportRules(ctx, user.ID, f.Rules, time.Now().Unix())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s: %d rules added, %d updated\n", args[1], created, updated)
	return err
}
