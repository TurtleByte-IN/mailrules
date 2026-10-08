package store

import (
	"errors"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
)

// tworld is two tenants, each with one user and one mailbox, plus a teammate in the first
// tenant and a second mailbox there: one the owner keeps private and one they share. It is
// built entirely through store helpers, since sign-in does not exist yet (MAI-133).
type tworld struct {
	s                *Store
	aUser, bUser     User    // owners in tenants A and B
	mate             User    // a second user in tenant A
	aPriv, aShared   Account // tenant A: a mailbox kept private and one shared with the team
	bAcct            Account // tenant B's mailbox
	aTenant, bTenant int64
}

func twoTenants(t *testing.T) tworld {
	t.Helper()
	s, _ := open(t)
	ctx := t.Context()
	w := tworld{s: s, aTenant: SelfHostTenant}
	var err error
	// Tenant A is the self-host tenant; its owner is the first user.
	if w.aUser, err = s.CreateFirstUser(ctx, "owner@a.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	if w.mate, err = s.CreateUser(ctx, w.aTenant, "mate@a.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	if w.bTenant, err = s.CreateTenant(ctx, "workos", "org_B", 1); err != nil {
		t.Fatal(err)
	}
	if w.bUser, err = s.CreateUser(ctx, w.bTenant, "owner@b.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	mk := func(owner User, user string) Account {
		a, err := s.CreateAccount(ctx, testMaster, Account{UserID: owner.ID, Label: user, Preset: "generic",
			Host: "imap.test", Port: 993, TLSMode: "implicit", Username: user, CreatedAt: 1}, "pw")
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	w.aPriv = mk(w.aUser, "priv@a.test")
	w.aShared = mk(w.aUser, "shared@a.test")
	if err := s.SetAccountShared(ctx, w.aShared.ID, true); err != nil {
		t.Fatal(err)
	}
	w.aShared.Shared = true
	w.bAcct = mk(w.bUser, "box@b.test")
	return w
}

// ingestDecided puts one email in a mailbox, decides it and records a done move, so the
// activity feed, stats, review queue and batches all have something to isolate.
func (w tworld) seed(t *testing.T, a Account, uid uint32, state string) int64 {
	t.Helper()
	ctx := t.Context()
	m, _, err := w.s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	d, err := w.s.AddDecision(ctx, Decision{MessageID: m.ID, Stage: "sender", CreatedAt: 1000}, state)
	if err != nil {
		t.Fatal(err)
	}
	if state == StateActed {
		if _, err := w.s.InsertAction(ctx, Action{DecisionID: d, MessageID: m.ID, AccountID: a.ID, Kind: "move",
			Folder: "Food", Status: ActionDone, CreatedAt: 1000, Before: Snapshot{Folder: "INBOX", UIDValidity: 1, UID: uid}}); err != nil {
			t.Fatal(err)
		}
	}
	return m.ID
}

// TestTenantIsolation proves one tenant cannot list, read or change another tenant's
// mailboxes, mail, activity, review queue, rules, sender rules, settings, usage, batches or
// stats. Every user-facing read goes through a Viewer, and the owning tenant sees its own
// while the other sees nothing.
func TestTenantIsolation(t *testing.T) {
	w := twoTenants(t)
	s, ctx := w.s, t.Context()
	av, bv := w.aUser.Viewer(), w.bUser.Viewer()

	// Mail, activity, review and stats seeded in both tenants.
	aMsg := w.seed(t, w.aPriv, 1, StateActed)
	bMsg := w.seed(t, w.bAcct, 1, StateActed)
	w.seed(t, w.aPriv, 2, StateReview)
	w.seed(t, w.bAcct, 2, StateReview)

	// Mailboxes: each tenant lists only its own; the other's is missing, not refused.
	if list, err := s.VisibleAccounts(ctx, av); err != nil || len(list) != 2 {
		t.Fatalf("A sees %d mailboxes, want its 2: %v", len(list), err)
	}
	if list, err := s.VisibleAccounts(ctx, bv); err != nil || len(list) != 1 || list[0].ID != w.bAcct.ID {
		t.Fatalf("B sees %+v, want only its own: %v", list, err)
	}
	if _, err := s.VisibleAccount(ctx, bv, w.aPriv.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("B read A's mailbox: %v", err)
	}
	if _, err := s.VisibleAccount(ctx, av, w.bAcct.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("A read B's mailbox: %v", err)
	}
	if ok, _ := s.CanSee(ctx, bv, w.aPriv.ID); ok {
		t.Error("CanSee let B see A's mailbox")
	}

	// Messages and activity.
	if _, err := s.VisibleMessage(ctx, bv, aMsg); !errors.Is(err, ErrNotFound) {
		t.Errorf("B read A's message: %v", err)
	}
	if _, err := s.ActivityFor(ctx, bv, aMsg); !errors.Is(err, ErrNotFound) {
		t.Errorf("B read A's activity row: %v", err)
	}
	if rows, err := s.Activity(ctx, bv, ActivityFilter{}); err != nil || len(rows) != 2 {
		t.Errorf("B's feed = %d rows, want only B's 2: %v", len(rows), err)
	}
	// Even naming A's account id in the filter, B sees nothing of it.
	if rows, _ := s.Activity(ctx, bv, ActivityFilter{AccountID: w.aPriv.ID}); len(rows) != 0 {
		t.Errorf("B's feed filtered to A's mailbox = %d rows, want 0", len(rows))
	}

	// Review queue.
	if n, err := s.CountMessages(ctx, av, StateReview); err != nil || n != 1 {
		t.Errorf("A review count = %d, want 1: %v", n, err)
	}
	if n, err := s.CountMessages(ctx, bv, StateReview); err != nil || n != 1 {
		t.Errorf("B review count = %d, want 1: %v", n, err)
	}

	// Rules and sender rules belong to the tenant.
	if _, err := s.CreateRule(ctx, w.aTenant, rules.Rule{UserID: w.aUser.ID, Name: "A rule",
		Actions: []rules.Action{{Type: rules.ActKeep}}, Enabled: true}, 1); err != nil {
		t.Fatal(err)
	}
	if list, err := s.Rules(ctx, w.bTenant); err != nil || len(list) != 0 {
		t.Errorf("B sees A's rules: %+v %v", list, err)
	}
	if _, err := s.PutSenderRule(ctx, w.aTenant, rules.SenderRule{UserID: w.aUser.ID,
		MatchType: rules.MatchDomain, Value: "a.test", Verdict: rules.VerdictKeep, Source: "user"}, 1); err != nil {
		t.Fatal(err)
	}
	if list, err := s.SenderRules(ctx, w.bTenant); err != nil || len(list) != 0 {
		t.Errorf("B sees A's sender rules: %+v %v", list, err)
	}

	// Settings and usage.
	if err := s.SetSetting(ctx, w.aTenant, "dry_run", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setting(ctx, w.bTenant, "dry_run"); !errors.Is(err, ErrNotFound) {
		t.Errorf("B read A's setting: %v", err)
	}
	if on, err := s.DryRun(ctx, w.bTenant, false); err != nil || on {
		t.Errorf("A's dry-run leaked into B: %v %v", on, err)
	}
	if err := s.AddUsage(ctx, w.aTenant, false, "2026-10-09", "openrouter", "jev", "decide", 1, 10, 1, 0.1); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Usage(ctx, w.bTenant, "2026-01-01"); err != nil || len(rows) != 0 {
		t.Errorf("B sees A's usage ledger: %+v %v", rows, err)
	}

	// Batches and stats.
	bid, err := s.CreateBatch(ctx, w.aTenant, BatchUndo, BatchDone, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VisibleBatch(ctx, bv, bid); !errors.Is(err, ErrNotFound) {
		t.Errorf("B read A's batch: %v", err)
	}
	if list, err := s.Batches(ctx, bv, "", 0, 50); err != nil || len(list) != 0 {
		t.Errorf("B lists A's batches: %+v %v", list, err)
	}
	if tot, err := s.StatsTotals(ctx, bv, 0); err != nil || tot.Processed != 2 {
		t.Errorf("B stats processed = %d, want only B's 2: %v", tot.Processed, err)
	}
	if tot, _ := s.StatsTotals(ctx, av, 0); tot.Processed != 2 {
		t.Errorf("A stats processed = %d, want only A's 2", tot.Processed)
	}
	_ = bMsg
}

// TestTeammateSeesSharedNotPrivate proves a teammate in the same tenant does not see a
// mailbox the owner keeps private, but does see a shared one and the mail in it, while a
// mailbox they do not own is theirs to read, not to manage (the store marks ownership;
// the owner-only check lives in the API).
func TestTeammateSeesSharedNotPrivate(t *testing.T) {
	w := twoTenants(t)
	s, ctx := w.s, t.Context()
	mate := w.mate.Viewer()

	priv := w.seed(t, w.aPriv, 1, StateActed)
	shared := w.seed(t, w.aShared, 1, StateActed)

	// The teammate sees only the shared mailbox.
	list, err := s.VisibleAccounts(ctx, mate)
	if err != nil || len(list) != 1 || list[0].ID != w.aShared.ID {
		t.Fatalf("teammate sees %+v, want only the shared mailbox: %v", list, err)
	}
	if _, err := s.VisibleAccount(ctx, mate, w.aPriv.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("teammate read the owner's private mailbox: %v", err)
	}
	if a, err := s.VisibleAccount(ctx, mate, w.aShared.ID); err != nil || a.UserID == w.mate.ID {
		t.Errorf("teammate sees the shared mailbox as theirs (owner=%d, mate=%d): %v", a.UserID, w.mate.ID, err)
	}

	// Its mail is the teammate's to read; the private mailbox's mail is not.
	if _, err := s.VisibleMessage(ctx, mate, shared); err != nil {
		t.Errorf("teammate could not read mail of the shared mailbox: %v", err)
	}
	if _, err := s.VisibleMessage(ctx, mate, priv); !errors.Is(err, ErrNotFound) {
		t.Errorf("teammate read mail of the private mailbox: %v", err)
	}
	if rows, err := s.Activity(ctx, mate, ActivityFilter{}); err != nil || len(rows) != 1 {
		t.Errorf("teammate feed = %d rows, want only the shared mailbox's 1: %v", len(rows), err)
	}

	// Sharing is the owner's to change; once private again the teammate loses it.
	if err := s.SetAccountShared(ctx, w.aShared.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VisibleAccount(ctx, mate, w.aShared.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("teammate still sees a mailbox the owner made private again: %v", err)
	}
}
