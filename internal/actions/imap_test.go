package actions

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// imapEnv is an executor over the real IMAP connector and go-imap's in-memory server.
func imapEnv(t *testing.T, caps ...goimap.Cap) (*env, *imaptest.Server) {
	t.Helper()
	set := goimap.CapSet{goimap.CapIMAP4rev1: {}}
	for _, c := range caps {
		set[c] = struct{}{}
	}
	srv := imaptest.Start(t, set)
	e := newStoreEnv(t)
	mb, err := imap.Open(t.Context(), imap.Config{
		AccountID: e.acct.ID, Host: srv.Host, Port: srv.Port, TLSMode: presets.TLSImplicit,
		Username: imaptest.Username, Password: imaptest.Password, TLSConfig: srv.TLS, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mb.Close() })
	e.accs.mb, e.raw = mb, mb
	return e, srv
}

// arrive appends an email to the server's INBOX and tracks it.
func (e *env) arrive(srv *imaptest.Server, id string, flags ...goimap.Flag) DecisionRecord {
	e.t.Helper()
	uid := srv.Append(e.t, "INBOX", eml(id), flags...)
	st, err := e.raw.Status(e.t.Context(), "INBOX")
	if err != nil {
		e.t.Fatal(err)
	}
	return e.track(mail.MsgRef{AccountID: e.acct.ID, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: uid}, id)
}

func (e *env) count(folder string) int {
	e.t.Helper()
	refs, _, err := e.raw.FetchSince(e.t.Context(), folder, time.Time{}, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(refs)
}

func TestUndoRoundTripOverIMAP(t *testing.T) {
	e, srv := imapEnv(t, goimap.CapMove, goimap.CapUIDPlus)
	ctx := t.Context()
	d := e.arrive(srv, "1", goimap.FlagFlagged, goimap.FlagAnswered)
	before := []string{`\Answered`, `\Flagged`}

	recs, err := e.x.Apply(ctx, d, act("read", "move:Food", "unflag"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if ref, flags := e.where(d.MessageID); ref.Folder != "Food" || !slices.Equal(flags, []string{`\Answered`, `\Seen`}) || e.count("INBOX") != 0 {
		t.Fatalf("after apply: in %s with %v, %d left in INBOX", ref.Folder, flags, e.count("INBOX"))
	}
	if !slices.Equal(recs[0].Before.Flags, before) || recs[2].Before.Folder != "Food" {
		t.Errorf("snapshots = %+v", recs)
	}

	// Undo each action, newest first: the folder and the flags come back exactly.
	for _, r := range slices.Backward(recs) {
		if err := e.x.Undo(ctx, r.ID); err != nil {
			t.Fatalf("undo %s: %v", r.Kind, err)
		}
	}
	ref, flags := e.where(d.MessageID)
	if ref.Folder != "INBOX" || !slices.Equal(flags, before) || e.count("Food") != 0 || e.count("INBOX") != 1 {
		t.Errorf("after undo: in %s with %v (want INBOX with %v); Food holds %d", ref.Folder, flags, before, e.count("Food"))
	}
	// Undoing twice is a no-op.
	for _, r := range recs {
		if err := e.x.Undo(ctx, r.ID); err != nil {
			t.Errorf("second undo of %s: %v", r.Kind, err)
		}
	}
	if again, flags2 := e.where(d.MessageID); again != ref || !slices.Equal(flags2, flags) {
		t.Errorf("a second undo changed the message: %+v %v", again, flags2)
	}
}

func TestUndoAfterUIDValidityChange(t *testing.T) {
	e, srv := imapEnv(t, goimap.CapMove, goimap.CapUIDPlus)
	ctx := t.Context()
	d := e.arrive(srv, "1", goimap.FlagSeen)
	recs, err := e.x.Apply(ctx, d, act("move:Food"), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The server rebuilds the folder: same mail, new UIDVALIDITY, so the stored UID is void.
	rebuild := func(restore bool) {
		t.Helper()
		if err := srv.User.Delete("Food"); err != nil {
			t.Fatal(err)
		}
		if err := srv.User.Create("Food", nil); err != nil {
			t.Fatal(err)
		}
		if restore {
			srv.Append(t, "Food", "X-Padding: so the UID differs too\r\n\r\n")
			srv.Append(t, "Food", eml("1"), goimap.FlagSeen)
		}
	}
	rebuild(true)
	if st, _ := e.raw.Status(ctx, "Food"); st.UIDValidity == recs[0].After.UIDValidity {
		t.Fatal("the test server kept the UIDVALIDITY; this test proves nothing")
	}
	if err := e.x.Undo(ctx, recs[0].ID); err != nil {
		t.Fatalf("undo by Message-ID: %v", err)
	}
	if ref, flags := e.where(d.MessageID); ref.Folder != "INBOX" || !slices.Equal(flags, []string{`\Seen`}) || e.count("Food") != 1 {
		t.Errorf("after undo: in %s with %v; Food holds %d (want only the padding message)", ref.Folder, flags, e.count("Food"))
	}

	// Rebuilt without the message: it is gone, and undo says so without touching anything.
	d2 := e.arrive(srv, "2")
	recs, err = e.x.Apply(ctx, d2, act("move:Food", "flag"), 0)
	if err != nil {
		t.Fatal(err)
	}
	e.count("INBOX") // leave Food: the in-memory server keeps a deleted folder alive for a session that has it selected
	rebuild(false)
	for _, r := range recs {
		if err := e.x.Undo(ctx, r.ID); !errors.Is(err, ErrGone) || err.Error() != "message was moved or deleted outside MailRules" {
			t.Errorf("undo %s of a deleted message = %v, want ErrGone", r.Kind, err)
		}
	}
	// Same UIDVALIDITY, but the user filed it elsewhere.
	d3 := e.arrive(srv, "3")
	if recs, err = e.x.Apply(ctx, d3, act("move:Food"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.raw.Move(ctx, recs[0].After.Ref(e.acct.ID), "INBOX"); err != nil { // by hand, not through the executor
		t.Fatal(err)
	}
	if err := e.x.Undo(ctx, recs[0].ID); !errors.Is(err, ErrGone) {
		t.Errorf("undo of a message filed elsewhere = %v, want ErrGone", err)
	}
	for _, id := range []int64{d2.MessageID, d3.MessageID} {
		for _, r := range e.actions(id) {
			if r.Status != store.ActionDone {
				t.Errorf("action %d is %s after a refused undo; it must stay done", r.ID, r.Status)
			}
		}
	}
}

// Safety invariant: nothing is expunged except the exact message just moved, even on a
// server without MOVE, where a move is COPY, \Deleted and UID EXPUNGE of that one UID.
func TestNoExpungeBeyondTheMovedMessage(t *testing.T) {
	e, srv := imapEnv(t, goimap.CapUIDPlus)
	ctx := t.Context()
	if err := srv.User.Create("Trash", nil); err != nil {
		t.Fatal(err)
	}
	if e.raw.Capabilities().Move {
		t.Fatal("the test server offers MOVE; the COPY fallback would not be exercised")
	}
	marked := srv.Append(t, "INBOX", eml("marked-by-another-client"), goimap.FlagDeleted)
	d := e.arrive(srv, "1")
	bystander := srv.Append(t, "INBOX", eml("bystander"))

	recs, err := e.x.Apply(ctx, d, act("trash"), 0)
	if err != nil {
		t.Fatal(err)
	}
	left, _, err := e.raw.FetchSince(ctx, "INBOX", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var uids []uint32
	for _, r := range left {
		uids = append(uids, r.UID)
	}
	if !slices.Equal(uids, []uint32{marked, bystander}) || e.count("Trash") != 1 {
		t.Fatalf("INBOX holds %v (want %v: only uid %d may leave); Trash holds %d", uids, []uint32{marked, bystander}, d.Ref.UID, e.count("Trash"))
	}
	// Trash was a move, so it can be undone; the mail marked \Deleted by someone else is still untouched.
	if err := e.x.Undo(ctx, recs[0].ID); err != nil {
		t.Fatal(err)
	}
	if ref, flags := e.where(d.MessageID); ref.Folder != "INBOX" || len(flags) != 0 || e.count("INBOX") != 3 || e.count("Trash") != 0 {
		t.Errorf("after undo: in %s with %v; INBOX %d, Trash %d", ref.Folder, flags, e.count("INBOX"), e.count("Trash"))
	}
	st, _ := e.raw.Status(ctx, "INBOX")
	if flags, err := e.raw.Flags(ctx, mail.MsgRef{AccountID: e.acct.ID, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: marked}); err != nil || !slices.Equal(flags, []string{`\Deleted`}) {
		t.Errorf("the other client's message: %v, %v", flags, err)
	}

	// Neither MOVE nor UIDPLUS: a move is refused outright rather than done unsafely.
	bare, srv2 := imapEnv(t)
	d = bare.arrive(srv2, "1")
	if _, err := bare.x.Apply(ctx, d, act("move:Food"), 0); !errors.Is(err, mail.ErrUnsupported) || bare.count("INBOX") != 1 {
		t.Errorf("move on a bare server = %v, INBOX holds %d", err, bare.count("INBOX"))
	}
	if rows := bare.actions(d.MessageID); len(rows) != 1 || rows[0].Status != store.ActionFailed {
		t.Errorf("rows = %+v", rows)
	}
}
