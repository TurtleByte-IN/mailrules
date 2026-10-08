package actions

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
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
// It holds for trash to the server's Trash and to MailRules Trash alike.
func TestNoExpungeBeyondTheMovedMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		toOwn  bool
		folder string
	}{{"to MailRules Trash", true, TrashFolder}, {"to the server's Trash", false, "Trash"}} {
		t.Run(tc.name, func(t *testing.T) {
			e, srv := imapEnv(t, goimap.CapUIDPlus)
			ctx := t.Context()
			if err := srv.User.Create("Trash", nil); err != nil {
				t.Fatal(err)
			}
			if err := e.st.SetTrashToFolder(ctx, 1, tc.toOwn); err != nil {
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
			if !slices.Equal(uids, []uint32{marked, bystander}) || e.count(tc.folder) != 1 {
				t.Fatalf("INBOX holds %v (want %v: only uid %d may leave); %s holds %d", uids, []uint32{marked, bystander}, d.Ref.UID, tc.folder, e.count(tc.folder))
			}
			// Trash was a move, so it can be undone; the mail marked \Deleted by someone else is still untouched.
			if err := e.x.Undo(ctx, recs[0].ID); err != nil {
				t.Fatal(err)
			}
			if ref, flags := e.where(d.MessageID); ref.Folder != "INBOX" || len(flags) != 0 || e.count("INBOX") != 3 || e.count(tc.folder) != 0 {
				t.Errorf("after undo: in %s with %v; INBOX %d, %s %d", ref.Folder, flags, e.count("INBOX"), tc.folder, e.count(tc.folder))
			}
			st, _ := e.raw.Status(ctx, "INBOX")
			if flags, err := e.raw.Flags(ctx, mail.MsgRef{AccountID: e.acct.ID, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: marked}); err != nil || !slices.Equal(flags, []string{`\Deleted`}) {
				t.Errorf("the other client's message: %v, %v", flags, err)
			}
		})
	}

	// Neither MOVE nor UIDPLUS: a move is refused outright rather than done unsafely.
	ctx := t.Context()
	bare, srv2 := imapEnv(t)
	d := bare.arrive(srv2, "1")
	if _, err := bare.x.Apply(ctx, d, act("move:Food"), 0); !errors.Is(err, mail.ErrUnsupported) || bare.count("INBOX") != 1 {
		t.Errorf("move on a bare server = %v, INBOX holds %d", err, bare.count("INBOX"))
	}
	if rows := bare.actions(d.MessageID); len(rows) != 1 || rows[0].Status != store.ActionFailed {
		t.Errorf("rows = %+v", rows)
	}
}

// trashEnv is imapEnv with the server's Trash and Junk folders in place (the stored folder
// list already marks them) and trash_to_folder as given.
func trashEnv(t *testing.T, toOwn bool) (*env, *imaptest.Server) {
	t.Helper()
	e, srv := imapEnv(t, goimap.CapMove, goimap.CapUIDPlus)
	for _, name := range []string{"Trash", "Junk"} {
		if err := srv.User.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.SetTrashToFolder(t.Context(), 1, toOwn); err != nil {
		t.Fatal(err)
	}
	return e, srv
}

// onServer reports whether the server has a folder by that name.
func (e *env) onServer(name string) bool {
	e.t.Helper()
	folders, err := e.raw.Folders(e.t.Context())
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.ContainsFunc(folders, func(f mail.Folder) bool { return f.Name == name })
}

// stored returns the account's stored folder by that name, and whether there is one.
func (e *env) stored(name string) (store.Folder, bool) {
	e.t.Helper()
	folders, err := e.st.Folders(e.t.Context(), e.acct.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	i := slices.IndexFunc(folders, func(f store.Folder) bool { return f.Name == name })
	if i < 0 {
		return store.Folder{}, false
	}
	return folders[i], true
}

// What one trash or junk action does under the trash_to_folder setting, dry-run and a
// server that refuses to make folders.
func TestTrashToFolder(t *testing.T) {
	tests := []struct {
		name        string
		toOwn       bool
		dry         bool
		refuse      bool // the server answers CREATE with NO
		act         string
		wantStatus  string
		wantFolder  string // where the email is afterwards
		wantRow     string // the row's folder
		wantErr     string // substring of the row's error
		wantCreated bool   // MailRules Trash exists on the server and in the stored list
	}{
		{"on: trash makes MailRules Trash and moves there", true, false, false, "trash", store.ActionDone, TrashFolder, TrashFolder, "", true},
		{"off: trash goes to the server's Trash", false, false, false, "trash", store.ActionDone, "Trash", "", "", false},
		{"on: junk still goes to Junk", true, false, false, "junk", store.ActionDone, "Junk", "", "", false},
		{"off: junk goes to Junk", false, false, false, "junk", store.ActionDone, "Junk", "", "", false},
		{"on, dry-run: nothing made or moved, the row says where", true, true, false, "trash", store.ActionDryRun, "INBOX", TrashFolder, "", false},
		{"on, the server refuses CREATE: fails, no fallback to Trash", true, false, true, "trash", store.ActionFailed, "INBOX", TrashFolder, `create folder "MailRules Trash"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, srv := trashEnv(t, tt.toOwn)
			ctx := t.Context()
			if err := e.st.SetDryRun(ctx, 1, tt.dry); err != nil {
				t.Fatal(err)
			}
			srv.RefuseCreate(tt.refuse)
			d := e.arrive(srv, "1")
			recs, err := e.x.Apply(ctx, d, act(tt.act), 0)
			if (err != nil) != (tt.wantStatus == store.ActionFailed) {
				t.Fatalf("Apply = %v", err)
			}
			rows := e.actions(d.MessageID)
			if len(rows) != 1 || len(recs) != 1 || rows[0].Kind != tt.act || rows[0].Status != tt.wantStatus ||
				rows[0].Folder != tt.wantRow || !strings.Contains(rows[0].Error, tt.wantErr) {
				t.Fatalf("rows = %+v, want one %s %s row naming %q", rows, tt.wantStatus, tt.act, tt.wantRow)
			}
			if ref, _ := e.where(d.MessageID); ref.Folder != tt.wantFolder || e.count(tt.wantFolder) != 1 {
				t.Errorf("the email is in %s (%d there), want %s", ref.Folder, e.count(tt.wantFolder), tt.wantFolder)
			}
			if tt.wantFolder != "Trash" && e.count("Trash") != 0 {
				t.Errorf("the server's Trash holds %d", e.count("Trash"))
			}
			f, stored := e.stored(TrashFolder)
			if e.onServer(TrashFolder) != tt.wantCreated || stored != tt.wantCreated || f.SpecialUse != "" {
				t.Errorf("MailRules Trash on the server %v, stored %v (%+v); want %v with no role", e.onServer(TrashFolder), stored, f, tt.wantCreated)
			}
		})
	}
}

// MailRules Trash is made once: a second trash finds it, and the mail already in it stays.
// Undo puts each email back where it came from.
func TestTrashToFolderMadeOnceAndUndone(t *testing.T) {
	e, srv := trashEnv(t, true)
	ctx := t.Context()
	first, second := e.arrive(srv, "1"), e.arrive(srv, "2")
	if _, err := e.x.Apply(ctx, first, act("trash"), 0); err != nil {
		t.Fatal(err)
	}
	made, err := e.raw.Status(ctx, TrashFolder)
	if err != nil {
		t.Fatal(err)
	}
	srv.RefuseCreate(true) // a second CREATE would now fail the action
	recs, err := e.x.Apply(ctx, second, act("trash"), 0)
	if err != nil {
		t.Fatalf("the second trash tried to make the folder again: %v", err)
	}
	if st, _ := e.raw.Status(ctx, TrashFolder); st.UIDValidity != made.UIDValidity || e.count(TrashFolder) != 2 {
		t.Errorf("MailRules Trash was rebuilt or lost mail: uidvalidity %d (was %d), holds %d", st.UIDValidity, made.UIDValidity, e.count(TrashFolder))
	}
	if recs[0].Kind != "trash" || recs[0].After.Folder != TrashFolder {
		t.Errorf("record = %+v", recs[0])
	}
	for _, d := range []DecisionRecord{second, first} {
		if _, u, why, err := e.x.UndoMessage(ctx, e.user.Viewer(), d.MessageID); err != nil || why != nil || u.Actions != 1 {
			t.Fatalf("undo: %+v, %v, %v", u, why, err)
		}
		if ref, _ := e.where(d.MessageID); ref.Folder != "INBOX" {
			t.Errorf("after undo the email is in %s, want INBOX", ref.Folder)
		}
	}
	if e.count(TrashFolder) != 0 || e.count("INBOX") != 2 {
		t.Errorf("after undo: MailRules Trash %d, INBOX %d", e.count(TrashFolder), e.count("INBOX"))
	}
}

// An email trashed to the server's Trash before the setting was turned on comes back from
// there: undo follows the folder the action recorded, not the setting in force.
func TestUndoOfARealTrashWithTheSettingOn(t *testing.T) {
	e, srv := trashEnv(t, false)
	ctx := t.Context()
	d := e.arrive(srv, "1")
	recs, err := e.x.Apply(ctx, d, act("trash"), 0)
	if err != nil || recs[0].After.Folder != "Trash" {
		t.Fatalf("trash with the setting off: %+v, %v", recs, err)
	}
	if err := e.st.SetTrashToFolder(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := e.x.Undo(ctx, recs[0].ID); err != nil {
		t.Fatal(err)
	}
	if ref, _ := e.where(d.MessageID); ref.Folder != "INBOX" || e.count("Trash") != 0 || e.onServer(TrashFolder) {
		t.Errorf("after undo: in %s, Trash holds %d, MailRules Trash made %v", ref.Folder, e.count("Trash"), e.onServer(TrashFolder))
	}
	if a := e.actions(d.MessageID)[0]; a.Status != store.ActionUndone {
		t.Errorf("action = %+v", a)
	}
}

// A trash_to_folder setting that cannot be read stops the actions before anything is
// recorded or changed, as an unreadable dry-run switch does.
func TestUnreadableTrashSetting(t *testing.T) {
	e, srv := trashEnv(t, true)
	ctx := t.Context()
	if err := e.st.SetSetting(ctx, 1, store.SettingTrashToFolder, "maybe"); err != nil {
		t.Fatal(err)
	}
	d := e.arrive(srv, "1")
	if _, err := e.x.Apply(ctx, d, act("trash"), 0); err == nil || len(e.actions(d.MessageID)) != 0 || e.count("INBOX") != 1 {
		t.Errorf("Apply = %v, rows %+v, INBOX %d", err, e.actions(d.MessageID), e.count("INBOX"))
	}
	// Without a trash action the setting is not read at all.
	if _, err := e.x.Apply(ctx, d, act("read"), 0); err != nil {
		t.Errorf("a read with an unreadable trash setting: %v", err)
	}
}

// An email waiting in Needs review that the user moved or deleted in their mail client
// (a second connection here) is not where MailRules saw it. Answering it takes it off the
// queue as skipped, touches no mailbox and says why; one still there is answered as usual.
func TestAnswerReviewForMailGoneElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(t *testing.T, c *imapclient.Client, uid goimap.UID)
		gone   bool
	}{
		{"moved to another folder", func(t *testing.T, c *imapclient.Client, uid goimap.UID) {
			if _, err := c.Move(goimap.UIDSetNum(uid), "Archive").Wait(); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"deleted", func(t *testing.T, c *imapclient.Client, uid goimap.UID) {
			store := &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}, Silent: true}
			if err := c.Store(goimap.UIDSetNum(uid), store, nil).Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.Expunge().Close(); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"still in the inbox", func(*testing.T, *imapclient.Client, goimap.UID) {}, false},
	} {
		for _, keep := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, keep %v", tc.name, keep), func(t *testing.T) {
				e, srv := imapEnv(t, goimap.CapMove, goimap.CapUIDPlus)
				ctx := t.Context()
				if err := srv.User.Create("Archive", nil); err != nil {
					t.Fatal(err)
				}
				food, err := e.st.CreateRule(ctx, 1, rules.Rule{UserID: e.user.ID, Name: "Food", Intent: "Food", Actions: act("move:Food"), Enabled: true}, 1)
				if err != nil {
					t.Fatal(err)
				}
				d := e.arrive(srv, "1")
				if _, err := e.st.AddDecision(ctx, store.Decision{MessageID: d.MessageID, Stage: "decider", RuleID: food.ID, Confidence: 0.4, CreatedAt: 1}, store.StateReview); err != nil {
					t.Fatal(err)
				}

				// The mail client: its own connection, as the user's phone would be.
				c, err := imapclient.DialTLS(net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port)), &imapclient.Options{TLSConfig: srv.TLS})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if err := c.Login(imaptest.Username, imaptest.Password).Wait(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Select("INBOX", nil).Wait(); err != nil {
					t.Fatal(err)
				}
				tc.client(t, c, goimap.UID(d.Ref.UID))

				right := food.ID
				if keep {
					right = 0
				}
				_, err = e.x.Correct(ctx, Correction{By: e.user.Viewer(), MessageID: d.MessageID, RightRuleID: right, Review: true})
				m, _ := e.st.Message(ctx, d.MessageID)
				corrs, _ := e.st.MessageCorrections(ctx, d.MessageID)
				if !tc.gone {
					if err != nil || m.State != store.StateActed || len(corrs) != 1 {
						t.Fatalf("answer = %v; state %s, %d corrections", err, m.State, len(corrs))
					}
					return
				}
				if !errors.Is(err, ErrLeftReview) || !errors.Is(err, ErrGone) {
					t.Errorf("answer = %v, want ErrLeftReview", err)
				}
				if m.State != store.StateSkipped || len(corrs) != 0 || len(e.actions(d.MessageID)) != 0 {
					t.Errorf("state %s, %d corrections, actions %+v; want skipped and nothing recorded", m.State, len(corrs), e.actions(d.MessageID))
				}
				if e.onServer("Food") || e.count("INBOX") != 0 {
					t.Errorf("the mailbox was touched: Food made %v, INBOX holds %d", e.onServer("Food"), e.count("INBOX"))
				}
			})
		}
	}
}
