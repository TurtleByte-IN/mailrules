package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store/migrations"
	"github.com/TurtleByte-IN/mailrules/internal/store/pgtest"
)

func TestRules(t *testing.T) {
	s, db := open(t)
	ctx := t.Context()
	u, _ := s.CreateFirstUser(ctx, "me@icloud.com", "hash", 100)
	threshold := 0.9

	food, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "food", Said: "Swiggy and Zomato go to Food", Priority: 20, Enabled: true,
		Conditions: rules.Cond{All: []rules.Cond{{Field: "from_domain", Op: rules.OpIn, Value: []any{"swiggy.in", "zomato.com"}}}},
		Exceptions: rules.Cond{Field: "subject", Op: rules.OpMatches, Value: "^refund"},
		Actions:    []rules.Action{{Type: rules.ActMove, Folder: "Food"}, {Type: rules.ActRead}}}, 100)
	if err != nil || food.ID == 0 || food.Version != 1 || food.CreatedAt != 100 {
		t.Fatalf("create: %+v %v", food, err)
	}
	scams, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: "scams", Template: "Cold sales", Intent: "Fake bank alerts", Priority: 10, Enabled: true,
		Model: "clef", MinConfidence: &threshold, Actions: []rules.Action{{Type: rules.ActTrash}}, Draft: &rules.Draft{Instruction: "polite no", ReadPrevious: true}}, 100)
	if err != nil {
		t.Fatal(err)
	}

	// Everything survives the trip, the optional columns included.
	for _, want := range []rules.Rule{food, scams} {
		got, err := s.Rule(ctx, u.ID, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Errorf("rule %d = %s\nwant %s", want.ID, a, b)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("stored rule no longer validates: %v", err)
		}
	}
	// Unset optional columns are NULL, as the schema documents, not empty strings.
	var nulls int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE id = ? AND account_id IS NULL AND intent IS NULL
		AND model IS NULL AND min_confidence IS NULL AND template IS NULL AND draft IS NULL AND exceptions != '{}'`, food.ID).Scan(&nulls); err != nil || nulls != 1 {
		t.Errorf("optional columns not NULL: %d %v", nulls, err)
	}

	list, err := s.Rules(ctx, u.TenantID)
	if err != nil || len(list) != 2 || list[0].ID != scams.ID || list[1].ID != food.ID {
		t.Fatalf("list not in priority order: %+v %v", list, err)
	}
	if other, err := s.Rules(ctx, u.TenantID+1); err != nil || len(other) != 0 {
		t.Fatalf("another tenant's rules: %+v %v", other, err)
	}

	food.Name, food.Enabled, food.Priority, food.Exceptions = "meals", false, 5, rules.Cond{}
	if err := s.UpdateRule(ctx, u.TenantID, food, 200); err != nil {
		t.Fatal(err)
	}
	got, err := s.Rule(ctx, u.TenantID, food.ID)
	if err != nil || got.Name != "meals" || got.Enabled || got.Version != 2 || got.UpdatedAt != 200 || got.CreatedAt != 100 || !got.Exceptions.IsEmpty() {
		t.Fatalf("after update: %+v %v", got, err)
	}
	if err := s.UpdateRule(ctx, u.TenantID, rules.Rule{ID: 999, UserID: u.ID, Name: "x"}, 200); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing rule: got %v", err)
	}
	if _, err := s.Rule(ctx, u.TenantID+1, food.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant's rule: got %v", err)
	}

	// Deleting a rule takes the sender rules that route to it along.
	if _, err := s.PutSenderRule(ctx, u.TenantID, rules.SenderRule{UserID: u.ID, MatchType: rules.MatchDomain, Value: "zomato.com",
		RuleID: food.ID, Verdict: rules.VerdictRoute, Source: "learned"}, 300); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule(ctx, u.TenantID, food.ID); err != nil {
		t.Fatal(err)
	}
	if srs, _ := s.SenderRules(ctx, u.TenantID); len(srs) != 0 {
		t.Errorf("sender rule outlived its rule: %+v", srs)
	}
	if err := s.DeleteRule(ctx, u.TenantID, food.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: got %v", err)
	}
}

// Migration 0007: a rule added from a template before it had its own column carried
// "Template: <name>" in said. The name moves to template; words the user added below it
// in a rewrite stay in said, and a said of the user's own is left alone.
func TestMigrationRuleTemplate(t *testing.T) {
	ctx := t.Context()
	db := migratedTo(t, 6)
	s := keyed(db)
	uid := legacyUser(t, db)
	ids := map[string]int64{}
	for name, said := range map[string]any{
		"template":  "Template: Receipts",
		"rewritten": "Template: Cold sales\nalso pitches about SEO",
		"own words": "Swiggy goes to Food",
		"no words":  nil,
	} {
		res, err := db.ExecContext(ctx, `INSERT INTO rules (user_id, name, said, actions, priority, created_at, updated_at) VALUES (?, ?, ?, '[{"type":"keep"}]', 1, 1, 1)`,
			uid, name, said)
		if err != nil {
			t.Fatal(err)
		}
		ids[name], _ = res.LastInsertId()
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][2]string{
		"template":  {"", "Receipts"},
		"rewritten": {"also pitches about SEO", "Cold sales"},
		"own words": {"Swiggy goes to Food", ""},
		"no words":  {"", ""},
	} {
		r, err := s.Rule(ctx, SelfHostTenant, ids[name])
		if err != nil || r.Said != want[0] || r.Template != want[1] {
			t.Errorf("%s: said %q, template %q, %v; want %q, %q", name, r.Said, r.Template, err, want[0], want[1])
		}
	}
}

// Migration 0033: a rule made in Build with conditions saved "Built with conditions" as
// said (MAI-261). It goes; words a rewrite added under it stay, and the owner's own words
// are left alone. On Postgres it runs as the owner, whom row-level security applies to.
func TestMigrationBuiltWithConditions(t *testing.T) {
	ctx := t.Context()
	s, owner := open(t)
	dialect, fsys := goose.DialectSQLite3, migrations.SQLite()
	if pgtest.DSN() != "" {
		pg := pgtest.Open(t)
		s, dialect, fsys = keyed(pg.App), goose.DialectPostgres, migrations.Postgres()
		var err error
		if owner, err = OpenPostgres(ctx, pg.URL(pgtest.AsOwner)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = owner.Close() })
	}
	u, _ := s.CreateFirstUser(ctx, "me@icloud.com", "hash", 100)
	cases := map[string][2]string{ // said before, said after
		"placeholder":     {"Built with conditions", ""},
		"rewritten":       {"Built with conditions\nalso mark them read", "also mark them read"},
		"owner's words":   {"Built with conditions for Swiggy", "Built with conditions for Swiggy"},
		"no words at all": {"", ""},
	}
	ids := map[string]int64{}
	for name, tc := range cases {
		r, err := s.CreateRule(ctx, u.TenantID, rules.Rule{UserID: u.ID, Name: name, Said: tc[0], Priority: 1, Enabled: true,
			Actions: []rules.Action{{Type: rules.ActKeep}}}, 100)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = r.ID
	}
	p, err := goose.NewProvider(dialect, owner, fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 32); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	for name, tc := range cases {
		if r, err := s.Rule(ctx, u.TenantID, ids[name]); err != nil || r.Said != tc[1] {
			t.Errorf("%s: said %q, %v; want %q", name, r.Said, err, tc[1])
		}
	}
}

func TestSenderRules(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	u, _ := s.CreateFirstUser(ctx, "me@icloud.com", "hash", 100)

	keep, err := s.PutSenderRule(ctx, u.TenantID, rules.SenderRule{UserID: u.ID, MatchType: rules.MatchAddress, Value: "alumni@college.edu",
		Verdict: rules.VerdictKeep, Source: "user"}, 100)
	if err != nil || keep.ID == 0 || keep.CreatedAt != 100 {
		t.Fatalf("put: %+v %v", keep, err)
	}
	// The same sender again replaces the verdict and keeps the row.
	block, err := s.PutSenderRule(ctx, u.TenantID, rules.SenderRule{UserID: u.ID, MatchType: rules.MatchAddress, Value: "alumni@college.edu",
		Verdict: rules.VerdictBlock, Source: "user"}, 200)
	if err != nil || block.ID != keep.ID || block.CreatedAt != 100 {
		t.Fatalf("replace: %+v %v", block, err)
	}
	list, err := s.SenderRules(ctx, u.TenantID)
	if err != nil || len(list) != 1 || list[0] != block {
		t.Fatalf("list = %+v, %v; want %+v", list, err, block)
	}
	// A move keeps its folder; whatever replaces it clears the folder again.
	move, err := s.PutSenderRule(ctx, u.TenantID, rules.SenderRule{UserID: u.ID, MatchType: rules.MatchAddress, Value: "alumni@college.edu",
		Verdict: rules.VerdictMove, Folder: "College", Source: "user"}, 300)
	if list, _ := s.SenderRules(ctx, u.TenantID); err != nil || len(list) != 1 || list[0] != move || list[0].Folder != "College" {
		t.Fatalf("move = %+v, %v; listed %+v", move, err, list)
	}
	if _, err := s.PutSenderRule(ctx, u.TenantID, block, 400); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.SenderRules(ctx, u.TenantID); len(list) != 1 || list[0].Verdict != rules.VerdictBlock || list[0].Folder != "" {
		t.Fatalf("after replacing the move: %+v", list)
	}
	for range 2 { // deleting twice is harmless
		if err := s.DeleteSenderRule(ctx, u.TenantID, rules.MatchAddress, "alumni@college.edu"); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := s.SenderRules(ctx, u.TenantID); len(list) != 0 {
		t.Errorf("after delete: %+v", list)
	}
}
