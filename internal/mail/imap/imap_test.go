package imap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/imap/imaptest"
	"github.com/TurtleByte-IN/mailrules/internal/mail/presets"
	"github.com/TurtleByte-IN/mailrules/internal/message"
)

var (
	capsMove    = imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}}
	capsUIDPlus = imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}}
	capsBare    = imap.CapSet{imap.CapIMAP4rev1: {}}
)

func eml(n int) string {
	return fmt.Sprintf("From: Sender <sender@example.test>\r\nTo: me@example.test\r\nSubject: message %d\r\n"+
		"Message-ID: <msg-%d@example.test>\r\n\r\nbody %d\r\n", n, n, n)
}

const withAttachment = "From: a@example.test\r\nSubject: invoice\r\nMessage-ID: <att@example.test>\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
	"--b\r\nContent-Type: application/pdf; name=\"Invoice.PDF\"\r\nContent-Disposition: attachment; filename=\"Invoice.PDF\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n" +
	"--b\r\nContent-Type: image/png; name=\"logo.png\"\r\nContent-Disposition: inline; filename=\"logo.png\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\niVBORw==\r\n--b--\r\n"

// config has every timer shrunk so reconnects and polls take milliseconds.
func config(s *imaptest.Server) Config {
	return Config{
		AccountID: 7, Host: s.Host, Port: s.Port, TLSMode: presets.TLSImplicit,
		Username: imaptest.Username, Password: imaptest.Password, TLSConfig: s.TLS,
		Logger:      slog.New(slog.DiscardHandler),
		IdleRestart: 40 * time.Millisecond, PollInterval: 10 * time.Millisecond,
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond, ThrottleBackoff: 20 * time.Millisecond,
	}
}

func open(t *testing.T, cfg Config) *Mailbox {
	t.Helper()
	m, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// watch runs Watch in the background and returns where arrivals show up.
func watch(t *testing.T, m *Mailbox, lastUID uint32) <-chan mail.NewMail {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out := make(chan mail.NewMail)
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx, "INBOX", lastUID, out) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Watch returned %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Watch did not stop when its context was cancelled")
		}
	})
	return out
}

// expect reads exactly the wanted UIDs, in order, and then checks that nothing else follows.
func expect(t *testing.T, out <-chan mail.NewMail, want ...uint32) []mail.MsgRef {
	t.Helper()
	var refs []mail.MsgRef
	for _, uid := range want {
		select {
		case nm := <-out:
			if nm.Ref.UID != uid || nm.Ref.Folder != "INBOX" || nm.Ref.AccountID != 7 {
				t.Fatalf("got %+v, want uid %d (all wanted: %v)", nm.Ref, uid, want)
			}
			refs = append(refs, nm.Ref)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for uid %d (all wanted: %v)", uid, want)
		}
	}
	select {
	case nm := <-out:
		t.Fatalf("unexpected extra message %+v", nm.Ref)
	case <-time.After(80 * time.Millisecond): // two idle restarts and several polls
	}
	return refs
}

func uids(t *testing.T, m *Mailbox, folder string) []uint32 {
	t.Helper()
	refs, _, err := m.FetchSince(t.Context(), folder, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []uint32
	for _, r := range refs {
		out = append(out, r.UID)
	}
	return out
}

func TestWatchDetectsArrival(t *testing.T) {
	for _, tc := range []struct {
		name string
		poll bool
	}{{"idle", false}, {"polling without IDLE", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := imaptest.Start(t, capsMove)
			s.Append(t, "INBOX", eml(1))
			s.Append(t, "INBOX", eml(2))
			cfg := config(s)
			cfg.forcePoll = tc.poll
			m := open(t, cfg)
			if m.Capabilities().Idle == tc.poll {
				t.Fatalf("Idle capability = %v with poll = %v", m.Capabilities().Idle, tc.poll)
			}

			// First connect: baseline to the newest message so old mail is left alone.
			st, err := m.Status(t.Context(), "INBOX")
			if err != nil || st.UIDNext != 3 || st.UIDValidity == 0 {
				t.Fatalf("status = %+v, %v", st, err)
			}
			out := watch(t, m, st.UIDNext-1)
			expect(t, out)
			s.Append(t, "INBOX", eml(3))
			refs := expect(t, out, 3)
			if refs[0].UIDValidity != st.UIDValidity {
				t.Errorf("uidvalidity = %d, want %d", refs[0].UIDValidity, st.UIDValidity)
			}
			s.Append(t, "INBOX", eml(4))
			s.Append(t, "INBOX", eml(5))
			expect(t, out, 4, 5)
		})
	}
}

func TestWatchCatchesUpFromLastUID(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	for n := 1; n <= 3; n++ {
		s.Append(t, "INBOX", eml(n))
	}
	expect(t, watch(t, open(t, config(s)), 1), 2, 3)
}

func TestWatchSurvivesDisconnect(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	s.Append(t, "INBOX", eml(1))
	out := watch(t, open(t, config(s)), 0)
	expect(t, out, 1) // by now the watcher is idling

	for round := range 3 {
		s.KillConnections()
		// These arrive while the watcher is disconnected or reconnecting.
		first := s.Append(t, "INBOX", eml(10+round))
		second := s.Append(t, "INBOX", eml(20+round))
		expect(t, out, first, second)
		// And this one once it is idling again.
		expect(t, out, s.Append(t, "INBOX", eml(30+round)))
	}
}

func TestWatchRebaselinesOnUIDValidityChange(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	s.Append(t, "INBOX", eml(1))
	cfg := config(s)
	cfg.BackoffMin, cfg.BackoffMax = 300*time.Millisecond, 300*time.Millisecond // room to rebuild the folder
	out := watch(t, open(t, cfg), 0)
	old := expect(t, out, 1)[0]

	// The server rebuilds the folder: same mail, new UIDVALIDITY, UIDs handed out afresh.
	s.KillConnections()
	if err := s.User.Delete("INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := s.User.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		s.Append(t, "INBOX", eml(n))
	}
	// The three re-numbered messages are not new mail and must not be reported.
	select {
	case nm := <-out:
		t.Fatalf("re-sorted old mail after a UIDVALIDITY change: %+v", nm.Ref)
	case <-time.After(time.Second):
	}
	s.Append(t, "INBOX", eml(4))
	got := expect(t, out, 4)[0]
	if got.UIDValidity == old.UIDValidity {
		t.Errorf("uidvalidity still %d after the folder was rebuilt", got.UIDValidity)
	}
}

func TestMove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		wantErr error
	}{
		{"UID MOVE", capsMove, nil},
		{"COPY, STORE and UID EXPUNGE fallback", capsUIDPlus, nil},
		{"neither MOVE nor UIDPLUS is refused", capsBare, mail.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := imaptest.Start(t, tc.caps)
			if err := s.User.Create("Food", nil); err != nil {
				t.Fatal(err)
			}
			s.Append(t, "INBOX", eml(1))
			// Another client marked this one deleted; moving a different message must not expunge it.
			s.Append(t, "INBOX", eml(2), imap.FlagDeleted)
			s.Append(t, "INBOX", eml(3))
			m := open(t, config(s))
			if got := m.Capabilities().CanMove(); got != (tc.wantErr == nil) {
				t.Errorf("CanMove = %v", got)
			}
			st, _ := m.Status(ctx, "INBOX")
			ref := mail.MsgRef{AccountID: 7, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: 1}

			moved, err := m.Move(ctx, ref, "Food")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if got := uids(t, m, "INBOX"); !slices.Equal(got, []uint32{1, 2, 3}) {
					t.Fatalf("INBOX after a refused move = %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := uids(t, m, "INBOX"); !slices.Equal(got, []uint32{2, 3}) {
				t.Fatalf("INBOX = %v, want [2 3]: only the moved UID may be expunged", got)
			}
			dst, _ := m.Status(ctx, "Food")
			if moved.Folder != "Food" || moved.UID != 1 || moved.UIDValidity != dst.UIDValidity || moved.AccountID != 7 {
				t.Fatalf("moved ref = %+v", moved)
			}
			found, err := m.FindByMessageID(ctx, "Food", "<msg-1@example.test>")
			if err != nil || found != moved {
				t.Fatalf("find moved message: %+v, %v", found, err)
			}
			// Moving back is what undo does.
			back, err := m.Move(ctx, moved, "INBOX")
			if err != nil || back.Folder != "INBOX" || back.UID != 4 {
				t.Fatalf("move back: %+v, %v", back, err)
			}
			if _, err := m.Move(ctx, moved, "INBOX"); !errors.Is(err, mail.ErrNotFound) {
				t.Errorf("moving a message that is gone: err = %v, want ErrNotFound", err)
			}
			if _, err := m.Move(ctx, back, "Nowhere"); !errors.Is(err, mail.ErrNoFolder) {
				t.Errorf("moving to a missing folder: err = %v, want ErrNoFolder", err)
			}
			if got := uids(t, m, "INBOX"); !slices.Equal(got, []uint32{2, 3, 4}) {
				t.Errorf("INBOX after failed moves = %v", got)
			}
		})
	}
}

// MoveMany moves the messages still there with one command and says where each went; one
// that is not there comes back zero, a stale UIDVALIDITY moves nothing, and mail another
// client marked deleted is never expunged.
func TestMoveMany(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caps    imap.CapSet
		wantErr error
	}{
		{"UID MOVE", capsMove, nil},
		{"UID MOVE without UIDPLUS finds them by Message-ID", imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}}, nil},
		{"COPY, STORE and UID EXPUNGE fallback", capsUIDPlus, nil},
		{"neither MOVE nor UIDPLUS is refused", capsBare, mail.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := imaptest.Start(t, tc.caps)
			if err := s.User.Create("Food", nil); err != nil {
				t.Fatal(err)
			}
			s.Append(t, "INBOX", eml(1))
			s.Append(t, "INBOX", eml(2), imap.FlagDeleted) // another client's: never expunged
			s.Append(t, "INBOX", eml(3))
			s.Append(t, "INBOX", eml(4))
			m := open(t, config(s))
			st, _ := m.Status(ctx, "INBOX")
			ref := func(uid uint32) mail.MsgRef {
				return mail.MsgRef{AccountID: 7, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: uid}
			}

			moved, err := m.MoveMany(ctx, []mail.MsgRef{ref(1), ref(3), ref(9), ref(4)}, "Food")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if got := uids(t, m, "INBOX"); !slices.Equal(got, []uint32{1, 2, 3, 4}) {
					t.Fatalf("INBOX after a refused move = %v", got)
				}
				return
			}
			if err != nil || len(moved) != 4 {
				t.Fatalf("MoveMany = %+v, %v", moved, err)
			}
			if got := uids(t, m, "INBOX"); !slices.Equal(got, []uint32{2}) {
				t.Fatalf("INBOX = %v, want [2]: only the moved UIDs may be expunged", got)
			}
			dst, _ := m.Status(ctx, "Food")
			for i, id := range []string{"msg-1", "msg-3", "", "msg-4"} {
				if id == "" {
					if moved[i] != (mail.MsgRef{}) {
						t.Errorf("a UID that was never there came back as %+v", moved[i])
					}
					continue
				}
				found, err := m.FindByMessageID(ctx, "Food", id+"@example.test")
				if err != nil || found != moved[i] || moved[i].UIDValidity != dst.UIDValidity {
					t.Errorf("%s: moved to %+v, found at %+v (%v)", id, moved[i], found, err)
				}
			}

			stale := ref(2)
			stale.UIDValidity++
			if moved, err := m.MoveMany(ctx, []mail.MsgRef{stale}, "Food"); err != nil || moved[0] != (mail.MsgRef{}) {
				t.Errorf("a stale UIDVALIDITY: %+v, %v", moved, err)
			}
			if got := uids(t, m, "INBOX"); !slices.Equal(got, []uint32{2}) {
				t.Errorf("INBOX after a stale move = %v", got)
			}
		})
	}
}

// Append saves a whole message with its flags and date and says where it went; Delete
// removes that one UID and nothing else, even mail another client marked deleted, and is
// refused, changing nothing, by a server without UIDPLUS.
func TestAppendAndDelete(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      imap.CapSet
		deleteErr error
	}{
		{"UIDPLUS", capsUIDPlus, nil},
		{"no UIDPLUS: never deleted", capsBare, mail.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := imaptest.Start(t, tc.caps)
			if err := s.User.Create("Drafts", nil); err != nil {
				t.Fatal(err)
			}
			s.Append(t, "Drafts", eml(1), imap.FlagDraft)
			s.Append(t, "Drafts", eml(2), imap.FlagDeleted) // another client's, waiting for its own expunge
			m := open(t, config(s))
			at := time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)
			ref, err := m.Append(ctx, "Drafts", []byte(eml(3)), []string{`\Draft`, `\Seen`}, at)
			if err != nil {
				t.Fatal(err)
			}
			st := mustStatus(t, m, "Drafts")
			if ref != (mail.MsgRef{AccountID: 7, Folder: "Drafts", UIDValidity: st.UIDValidity, UID: 3}) {
				t.Fatalf("appended ref = %+v", ref)
			}
			raw, err := m.Fetch(ctx, ref, 0)
			if err != nil || !strings.Contains(string(raw.Header), "Subject: message 3") || !raw.InternalDate.Equal(at) {
				t.Fatalf("appended message = %+v, %v", raw, err)
			}
			if flags, _ := m.Flags(ctx, ref); !slices.Equal(flags, []string{`\Draft`, `\Seen`}) {
				t.Errorf("flags = %v", flags)
			}
			if _, err := m.Append(ctx, "Nowhere", []byte(eml(4)), nil, at); !errors.Is(err, mail.ErrNoFolder) {
				t.Errorf("append to a missing folder: %v", err)
			}

			err = m.Delete(ctx, ref)
			if !errors.Is(err, tc.deleteErr) {
				t.Fatalf("delete = %v, want %v", err, tc.deleteErr)
			}
			want := []uint32{1, 2}
			if tc.deleteErr != nil {
				want = []uint32{1, 2, 3}
			}
			if got := uids(t, m, "Drafts"); !slices.Equal(got, want) {
				t.Fatalf("Drafts after delete = %v, want %v: only the one UID may go", got, want)
			}
			if tc.deleteErr == nil {
				if err := m.Delete(ctx, ref); !errors.Is(err, mail.ErrNotFound) {
					t.Errorf("deleting it again: %v, want ErrNotFound", err)
				}
			}
		})
	}
}

func TestFetchLeavesMailUnread(t *testing.T) {
	ctx := t.Context()
	s := imaptest.Start(t, capsMove)
	s.Append(t, "INBOX", eml(1))
	s.Append(t, "INBOX", withAttachment)
	m := open(t, config(s))
	st, _ := m.Status(ctx, "INBOX")
	ref := mail.MsgRef{AccountID: 7, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: 1}

	raw, err := m.Fetch(ctx, ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw.Header), "Subject: message 1") || string(raw.Text) != "body 1\r\n" {
		t.Errorf("header = %q, text = %q", raw.Header, raw.Text)
	}
	if raw.Size != int64(len(eml(1))) || raw.InternalDate.IsZero() || raw.HasAttachment {
		t.Errorf("raw = %+v", raw)
	}
	if short, err := m.Fetch(ctx, ref, 4); err != nil || string(short.Text) != "body" {
		t.Errorf("fetch with maxBody 4: %q, %v", short.Text, err)
	}
	flags, err := m.Flags(ctx, ref)
	if err != nil || slices.Contains(flags, `\Seen`) || slices.Contains(raw.Flags, `\Seen`) {
		t.Fatalf("fetching marked the message read: flags = %v, %v", flags, err)
	}

	att := ref
	att.UID = 2
	raw, err = m.Fetch(ctx, att, 0)
	if err != nil || !raw.HasAttachment || !slices.Equal(raw.AttachmentExts, []string{"pdf"}) {
		t.Errorf("attachment: %v %v, %v", raw.HasAttachment, raw.AttachmentExts, err)
	}

	if err := m.SetFlags(ctx, ref, []string{`\Seen`, `\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	if flags, _ := m.Flags(ctx, ref); !slices.Equal(flags, []string{`\Flagged`, `\Seen`}) {
		t.Errorf("flags after add = %v", flags)
	}
	if err := m.SetFlags(ctx, ref, nil, []string{`\Seen`}); err != nil {
		t.Fatal(err)
	}
	if flags, _ := m.Flags(ctx, ref); !slices.Equal(flags, []string{`\Flagged`}) {
		t.Errorf("flags after remove = %v", flags)
	}

	// A reference from before a UIDVALIDITY change must never reach another message.
	stale := ref
	stale.UIDValidity++
	if _, err := m.Fetch(ctx, stale, 0); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("stale uidvalidity: err = %v, want ErrNotFound", err)
	}
	if err := m.SetFlags(ctx, stale, []string{`\Seen`}, nil); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("stale uidvalidity: err = %v, want ErrNotFound", err)
	}
	gone := ref
	gone.UID = 99
	if _, err := m.Fetch(ctx, gone, 0); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("missing uid: err = %v, want ErrNotFound", err)
	}
	gone.Folder = "Nowhere"
	if _, err := m.Fetch(ctx, gone, 0); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("missing folder: err = %v, want ErrNoFolder", err)
	}
}

// FetchMany reads several messages in one request: the result lines up with the refs asked
// for, a message that is gone comes back nil, and nothing is marked read.
func TestFetchManyLinesUpWithRefs(t *testing.T) {
	ctx := t.Context()
	s := imaptest.Start(t, capsMove)
	for i := 1; i <= 3; i++ {
		s.Append(t, "INBOX", eml(i))
	}
	m := open(t, config(s))
	st, _ := m.Status(ctx, "INBOX")
	ref := func(uid uint32) mail.MsgRef {
		return mail.MsgRef{AccountID: 7, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: uid}
	}

	raws, err := m.FetchMany(ctx, []mail.MsgRef{ref(3), ref(99), ref(1)}, 4)
	if err != nil || len(raws) != 3 {
		t.Fatalf("fetch many = %d raws, %v", len(raws), err)
	}
	if raws[0] == nil || !strings.Contains(string(raws[0].Header), "Subject: message 3") || string(raws[0].Text) != "body" {
		t.Errorf("first = %+v, want message 3 with its text cut to 4 bytes", raws[0])
	}
	if raws[1] != nil {
		t.Errorf("a missing uid came back as %+v, want nil", raws[1])
	}
	if raws[2] == nil || !strings.Contains(string(raws[2].Header), "Subject: message 1") || raws[2].InternalDate.IsZero() {
		t.Errorf("third = %+v, want message 1", raws[2])
	}
	for _, uid := range []uint32{1, 3} {
		if flags, err := m.Flags(ctx, ref(uid)); err != nil || slices.Contains(flags, `\Seen`) {
			t.Fatalf("fetching marked message %d read: flags = %v, %v", uid, flags, err)
		}
	}

	// A batch from before a UIDVALIDITY change must never reach other messages.
	stale := ref(1)
	stale.UIDValidity++
	if raws, err := m.FetchMany(ctx, []mail.MsgRef{stale}, 0); err != nil || raws[0] != nil {
		t.Errorf("stale uidvalidity = %+v, %v; want nil", raws, err)
	}
	if _, err := m.FetchMany(ctx, []mail.MsgRef{ref(1), stale}, 0); err == nil {
		t.Error("refs of two UIDVALIDITYs were fetched as one batch")
	}
	gone := ref(1)
	gone.Folder = "Nowhere"
	if _, err := m.FetchMany(ctx, []mail.MsgRef{gone}, 0); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("missing folder: err = %v, want ErrNoFolder", err)
	}
}

// A server like Dovecot shows a session mail that arrived after it selected the folder only
// once it next syncs: until then UID FETCH of the new UID answers nothing. The worker must
// catch up before calling the message gone (MAI-191), and a message that is truly gone is
// still not found.
func TestWorkerSeesMailArrivedSinceSelect(t *testing.T) {
	tests := []struct {
		name string
		run  func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error
	}{
		{"fetch", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			raw, err := m.Fetch(ctx, ref, 0)
			if err == nil && !strings.Contains(string(raw.Header), "Subject: message 2") {
				return fmt.Errorf("fetched %q", raw.Header)
			}
			return err
		}},
		{"flags", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			_, err := m.Flags(ctx, ref)
			return err
		}},
		{"move", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			_, err := m.Move(ctx, ref, "Food")
			return err
		}},
		{"fetch many", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			raws, err := m.FetchMany(ctx, []mail.MsgRef{ref}, 0)
			if err == nil && raws[0] == nil {
				return mail.ErrNotFound
			}
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s := imaptest.Start(t, capsMove)
			s.DelayNewMail(true)
			if err := s.User.Create("Food", nil); err != nil {
				t.Fatal(err)
			}
			s.Append(t, "INBOX", eml(1))
			m := open(t, config(s))
			st := mustStatus(t, m, "INBOX")
			ref := mail.MsgRef{AccountID: 7, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: 1}
			if _, err := m.Fetch(ctx, ref, 0); err != nil { // the worker now has INBOX selected
				t.Fatal(err)
			}
			ref.UID = s.Append(t, "INBOX", eml(2))
			if err := tt.run(ctx, m, ref); err != nil {
				t.Errorf("%s of a message that arrived since the folder was selected: %v", tt.name, err)
			}
			ref.UID = 99
			if err := tt.run(ctx, m, ref); !errors.Is(err, mail.ErrNotFound) {
				t.Errorf("%s of a missing uid: err = %v, want ErrNotFound", tt.name, err)
			}
		})
	}
}

// UID STORE and UID SEARCH answer OK without touching or finding mail the session has not
// been told of yet, so there is no empty answer to retry on: the worker must sync before
// sending them to a folder it already had selected (MAI-202).
func TestWorkerActsOnMailArrivedSinceSelect(t *testing.T) {
	tests := []struct {
		name string
		run  func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error
	}{
		{"set flags", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			if err := m.SetFlags(ctx, ref, []string{`\Flagged`}, nil); err != nil {
				return err
			}
			flags, err := m.Flags(ctx, ref)
			if err == nil && !slices.Contains(flags, `\Flagged`) {
				return fmt.Errorf("flags = %v, want \\Flagged among them", flags)
			}
			return err
		}},
		{"find by message-id", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			got, err := m.FindByMessageID(ctx, "INBOX", "<msg-2@example.test>")
			if err == nil && got != ref {
				return fmt.Errorf("found %+v, want %+v", got, ref)
			}
			return err
		}},
		{"fetch since", func(ctx context.Context, m *Mailbox, ref mail.MsgRef) error {
			refs, matched, err := m.FetchSince(ctx, "INBOX", time.Time{}, 0)
			if err == nil && (matched != 2 || !slices.Contains(refs, ref)) {
				return fmt.Errorf("found %d: %+v, want %+v among them", matched, refs, ref)
			}
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s := imaptest.Start(t, capsMove)
			s.DelayNewMail(true)
			s.Append(t, "INBOX", eml(1))
			m := open(t, config(s))
			st := mustStatus(t, m, "INBOX")
			ref := mail.MsgRef{AccountID: 7, Folder: "INBOX", UIDValidity: st.UIDValidity, UID: 1}
			if err := m.SetFlags(ctx, ref, []string{`\Seen`}, nil); err != nil { // INBOX is now selected
				t.Fatal(err)
			}
			ref.UID = s.Append(t, "INBOX", eml(2))
			if err := tt.run(ctx, m, ref); err != nil {
				t.Errorf("%s on a message that arrived since the folder was selected: %v", tt.name, err)
			}
		})
	}
}

// Without IDLE, Watch searches for new mail each poll. The first poll after mail arrived
// must find it, not only a later one, once an earlier search has synced the session.
func TestWatchPollSeesMailArrivedSinceLastSearch(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	s.DelayNewMail(true)
	s.Append(t, "INBOX", eml(1))
	tick := make(chan time.Time)
	cfg := config(s)
	cfg.forcePoll, cfg.pollTick = true, tick
	out := watch(t, open(t, cfg), 0)
	expect(t, out, 1) // the search on connect has run; no poll happens until the next tick
	uid := s.Append(t, "INBOX", eml(2))
	select { // exactly one poll
	case tick <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("Watch is not waiting to poll")
	}
	select {
	case nm := <-out:
		if nm.Ref.UID != uid {
			t.Fatalf("got uid %d, want %d", nm.Ref.UID, uid)
		}
	case <-time.After(5 * time.Second): // no further poll will come: the first one missed it
		t.Fatal("the first poll after the message arrived did not find it")
	}
}

// With IDLE, mail delivered after Watch's last search but before the server is idling
// still wakes it, on a connection's first IDLE and on every later one: the server
// announces as IDLE begins what changed since the last command, as Dovecot does
// (MAI-241, MAI-245). IdleRestart is an hour, so nothing else could wake Watch in time.
func TestWatchIdleSeesMailDeliveredAsIdleBegins(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	idling, finished := make(chan chan struct{}), make(chan struct{})
	s.OnIdle(func() { // hands the test each IDLE, and holds it until the test has delivered
		delivered := make(chan struct{})
		select {
		case idling <- delivered:
			select {
			case <-delivered:
			case <-finished:
			}
		case <-finished:
		}
	})
	cfg := config(s)
	cfg.IdleRestart = time.Hour
	out := watch(t, open(t, cfg), 0)
	t.Cleanup(func() { close(finished) })
	for round := 1; round <= 2; round++ { // the IDLE after connecting, then the one after a wake-up
		var delivered chan struct{}
		select {
		case delivered = <-idling:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: Watch never began IDLE", round)
		}
		uid := s.Append(t, "INBOX", eml(round))
		close(delivered)
		expect(t, out, uid)
	}
}

func TestFoldersAndEnsureFolder(t *testing.T) {
	ctx := t.Context()
	s := imaptest.Start(t, capsMove)
	for _, name := range []string{"Sent Messages", "Junk", "Deleted Messages", "Spam", "Notes"} {
		if err := s.User.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config(s)
	cfg.Preset, _ = presets.Get("icloud")
	m := open(t, cfg)

	role := func() map[string]string {
		t.Helper()
		folders, err := m.Folders(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range folders {
			if f.Delimiter != "/" {
				t.Errorf("%s: delimiter = %q", f.Name, f.Delimiter)
			}
			out[f.Name] = f.SpecialUse
		}
		return out
	}
	// No SPECIAL-USE on this server, so roles come from the iCloud preset's names.
	// "Junk" outranks "Spam" there, so only one folder gets the junk role.
	want := map[string]string{"INBOX": "", "Notes": "", "Spam": "", "Junk": mail.RoleJunk,
		"Sent Messages": mail.RoleSent, "Deleted Messages": mail.RoleTrash}
	if got := role(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("folders = %v, want %v", got, want)
	}

	for range 2 { // the second call finds everything in place
		if err := m.EnsureFolder(ctx, "Receipts/2026"); err != nil {
			t.Fatal(err)
		}
	}
	got := role()
	for _, name := range []string{"Receipts", "Receipts/2026"} {
		if _, ok := got[name]; !ok {
			t.Errorf("%s was not created: %v", name, got)
		}
	}
}

func TestToFoldersPrefersServerRoles(t *testing.T) {
	icloud, _ := presets.Get("icloud")
	got := toFolders([]*imap.ListData{
		{Mailbox: "INBOX", Delim: '.'},
		{Mailbox: "Parent", Delim: '.', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}},
		{Mailbox: "Papierkorb", Delim: '.', Attrs: []imap.MailboxAttr{imap.MailboxAttrHasNoChildren, imap.MailboxAttrTrash}},
		{Mailbox: "Deleted Messages", Delim: '.'},
		{Mailbox: "Archive"},
	}, icloud)
	want := []mail.Folder{
		{Name: "INBOX", Delimiter: "."},
		{Name: "Papierkorb", Delimiter: ".", SpecialUse: mail.RoleTrash},
		{Name: "Deleted Messages", Delimiter: "."}, // the server already named its trash
		{Name: "Archive", SpecialUse: mail.RoleArchive},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestSearchAndContactsScan(t *testing.T) {
	ctx := t.Context()
	s := imaptest.Start(t, capsMove)
	if err := s.User.Create("Sent", nil); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 4; n++ {
		s.Append(t, "INBOX", eml(n))
	}
	s.Append(t, "Sent", "To: Old <old@example.test>\r\nSubject: outside the limit\r\n\r\nx\r\n")
	s.Append(t, "Sent", "To: Ann <Ann@Example.test>, bob@example.test\r\nCc: carol@example.test\r\nSubject: hi\r\n\r\nsecret body\r\n")
	s.Append(t, "Sent", "To: bob@example.test\r\nSubject: again\r\n\r\nx\r\n")
	m := open(t, config(s))

	refs, matched, err := m.FetchSince(ctx, "INBOX", time.Now().Add(-48*time.Hour), 2)
	if err != nil || len(refs) != 2 || matched != 4 || refs[0].UID != 3 || refs[1].UID != 4 || refs[0].UIDValidity == 0 {
		t.Errorf("FetchSince newest two = %+v, matched %d, %v", refs, matched, err)
	}
	if refs, matched, err := m.FetchSince(ctx, "INBOX", time.Now().Add(48*time.Hour), 0); err != nil || len(refs) != 0 || matched != 0 {
		t.Errorf("FetchSince in the future = %+v, matched %d, %v", refs, matched, err)
	}
	if ref, err := m.FindByMessageID(ctx, "INBOX", "msg-3@example.test"); err != nil || ref.UID != 3 {
		t.Errorf("FindByMessageID = %+v, %v", ref, err)
	}
	if _, err := m.FindByMessageID(ctx, "INBOX", "<nope@example.test>"); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("FindByMessageID for a missing id: err = %v", err)
	}
	if _, err := m.Status(ctx, "Nowhere"); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("Status of a missing folder: err = %v", err)
	}

	got, err := m.SentRecipients(ctx, "Sent", 2)
	if err != nil {
		t.Fatal(err)
	}
	var addrs []string
	for _, r := range got {
		if r.SentAt.IsZero() {
			t.Errorf("%s has no sent time", r.Address)
		}
		addrs = append(addrs, r.Address)
	}
	if want := []string{"ann@example.test", "bob@example.test", "carol@example.test"}; !slices.Equal(addrs, want) {
		t.Errorf("recipients = %v, want %v", addrs, want)
	}
	// The scan must not have marked sent mail read or left a read-only selection behind.
	if err := m.SetFlags(ctx, mail.MsgRef{Folder: "Sent", UIDValidity: mustStatus(t, m, "Sent").UIDValidity, UID: 2}, []string{`\Flagged`}, nil); err != nil {
		t.Errorf("write after the read-only scan: %v", err)
	}
	if _, err := m.SentRecipients(ctx, "Nowhere", 10); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("scan of a missing folder: err = %v", err)
	}
}

// Present sees what another mail app did since the worker last had the folder selected: a
// message it moved away is gone, one it marked deleted is gone, and the rest are there,
// still unread.
func TestPresent(t *testing.T) {
	ctx := t.Context()
	s := imaptest.Start(t, capsMove)
	if err := s.User.Create("Receipts", nil); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		s.Append(t, "Receipts", eml(n))
	}
	m := open(t, config(s))
	validity := mustStatus(t, m, "Receipts").UIDValidity
	if got, err := m.Present(ctx, "Receipts", validity, []uint32{1, 2, 3, 9}); err != nil || !slices.Equal(got, []uint32{1, 2, 3}) {
		t.Fatalf("Present before = %v, %v", got, err)
	}

	other := open(t, config(s)) // another mail app, on its own connection
	if _, err := other.Move(ctx, mail.MsgRef{Folder: "Receipts", UIDValidity: validity, UID: 2}, "INBOX"); err != nil {
		t.Fatal(err)
	}
	if err := other.SetFlags(ctx, mail.MsgRef{Folder: "Receipts", UIDValidity: validity, UID: 3}, []string{`\Deleted`}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Present(ctx, "Receipts", validity, []uint32{1, 2, 3}); err != nil || !slices.Equal(got, []uint32{1}) {
		t.Errorf("Present after = %v, %v; want [1]", got, err)
	}
	if flags, err := m.Flags(ctx, mail.MsgRef{Folder: "Receipts", UIDValidity: validity, UID: 1}); err != nil || slices.Contains(flags, `\Seen`) {
		t.Errorf("flags after Present = %v, %v", flags, err)
	}
	if _, err := m.Present(ctx, "Receipts", validity+1, []uint32{1}); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("Present with an old UIDVALIDITY: err = %v", err)
	}
	if _, err := m.Present(ctx, "Nowhere", validity, []uint32{1}); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("Present in a missing folder: err = %v", err)
	}
	if got, err := m.Present(ctx, "Receipts", validity, nil); err != nil || got != nil {
		t.Errorf("Present of nothing = %v, %v", got, err)
	}
}

func mustStatus(t *testing.T, m *Mailbox, folder string) mail.FolderStatus {
	t.Helper()
	st, err := m.Status(t.Context(), folder)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestConnectErrors(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want error
	}{
		{"wrong password", func(c *Config) { c.Password = "wrong-secret-pw" }, mail.ErrAuth},
		{"unknown user", func(c *Config) { c.Username = "nobody@example.test" }, mail.ErrAuth},
		{"untrusted certificate", func(c *Config) { c.TLSConfig = nil }, mail.ErrTLS},
		{"nothing listening", func(c *Config) { c.Port = 1 }, mail.ErrConnection},
		{"plaintext is not offered", func(c *Config) { c.TLSMode = "none" }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(s)
			tc.edit(&cfg)
			_, err := Open(t.Context(), cfg)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), cfg.Password) {
				t.Fatalf("the password leaked into the error: %v", err)
			}
		})
	}
}

func TestCertificates(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	fp := s.Fingerprint()
	other := mail.Fingerprint([]byte("another certificate"))
	untrusted := &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12} // trusts nothing
	for _, tc := range []struct {
		name       string
		tls        *tls.Config
		serverName string
		pin        string
		wantPinned string // on a *mail.CertError; "" with wantErr means untrusted
		wantReason string
		wantErr    bool
	}{
		{name: "trusted chain", tls: s.TLS},
		{name: "unknown certificate, first time", tls: untrusted, wantErr: true, wantReason: "self-made"},
		{name: "trusted chain, another name", tls: s.TLS, serverName: "imap.example.test", wantErr: true, wantReason: "another name than imap.example.test"},
		{name: "pinned match", tls: untrusted, pin: fp},
		{name: "pinned match, another name", tls: untrusted, serverName: "imap.example.test", pin: fp},
		{name: "pinned mismatch", tls: untrusted, pin: other, wantErr: true, wantPinned: other},
		{name: "a pin wins over a trusted chain", tls: s.TLS, pin: other, wantErr: true, wantPinned: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(s)
			cfg.TLSConfig = tc.tls.Clone()
			cfg.TLSConfig.ServerName = tc.serverName
			cfg.CertFingerprint = tc.pin
			m, err := Open(t.Context(), cfg)
			if !tc.wantErr {
				if err != nil {
					t.Fatal(err)
				}
				_ = m.Close()
				return
			}
			var ce *mail.CertError
			if !errors.As(err, &ce) || !errors.Is(err, mail.ErrTLS) {
				t.Fatalf("err = %v, want a certificate error", err)
			}
			if ce.Cert.Fingerprint != fp || ce.Pinned != tc.wantPinned || !strings.Contains(ce.Reason, tc.wantReason) ||
				ce.Cert.Subject != "CN=imaptest" || ce.Cert.NotAfter.IsZero() {
				t.Errorf("certificate error = %+v", ce)
			}
		})
	}
	if err := verifyCert(tls.ConnectionState{}, nil, "x", ""); !errors.Is(err, mail.ErrTLS) {
		t.Errorf("no certificate at all: err = %v", err)
	}
}

// A server whose certificate is replaced after it was pinned is refused, on a new
// connection and by a running Watch alike, and Watch gives up instead of retrying.
func TestPinnedCertificateChanges(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	cfg := config(s)
	cfg.TLSConfig = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	cfg.CertFingerprint = s.Fingerprint()
	m := open(t, cfg)
	pinned := s.Fingerprint()
	s.Rotate(t)
	s.KillConnections()
	err := m.Watch(t.Context(), "INBOX", 0, make(chan mail.NewMail))
	var ce *mail.CertError
	if !errors.As(err, &ce) || ce.Pinned != pinned || ce.Cert.Fingerprint != s.Fingerprint() {
		t.Fatalf("Watch err = %v, want the changed certificate", err)
	}
	if _, err := Open(t.Context(), cfg); !errors.As(err, &ce) {
		t.Fatalf("Open err = %v, want the changed certificate", err)
	}
	cfg.CertFingerprint = s.Fingerprint() // the new one accepted
	_ = open(t, cfg)
}

func TestWatchStopsOnAuthFailure(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	m := open(t, config(s))
	m.cfg.Password = "revoked-app-password" // the app password was revoked after Open
	err := m.Watch(t.Context(), "INBOX", 0, make(chan mail.NewMail))
	if !errors.Is(err, mail.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth without retrying", err)
	}
	if strings.Contains(err.Error(), m.cfg.Password) {
		t.Fatalf("the password leaked into the error: %v", err)
	}
}

func TestWorkerReconnects(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	s.Append(t, "INBOX", eml(1))
	m := open(t, config(s))
	if got := uids(t, m, "INBOX"); len(got) != 1 {
		t.Fatalf("uids = %v", got)
	}
	s.KillConnections()
	// The first call after a drop may still see the dead connection; the one after redials.
	var err error
	for range 50 {
		if _, _, err = m.FetchSince(t.Context(), "INBOX", time.Time{}, 0); err == nil {
			return
		}
		if !errors.Is(err, mail.ErrConnection) {
			t.Fatalf("err = %v, want ErrConnection", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker never reconnected: %v", err)
}

func TestCancelledContext(t *testing.T) {
	s := imaptest.Start(t, capsMove)
	m := open(t, config(s))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Folders(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if _, err := m.Folders(t.Context()); err != nil { // and the mailbox still works afterwards
		t.Errorf("after a cancelled call: %v", err)
	}
}

// The whole read path on a real fixture: IMAP fetch, then parsing what came back.
func TestFetchAndParseFixture(t *testing.T) {
	ctx := t.Context()
	fixture, err := os.ReadFile("../../../testdata/attachment.eml")
	if err != nil {
		t.Fatal(err)
	}
	s := imaptest.Start(t, capsMove)
	s.Append(t, "INBOX", strings.ReplaceAll(string(fixture), "\n", "\r\n"))
	m := open(t, config(s))
	raw, err := m.Fetch(ctx, mail.MsgRef{Folder: "INBOX", UIDValidity: mustStatus(t, m, "INBOX").UIDValidity, UID: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := message.Parse(raw, 7, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.HasAttachment || !slices.Equal(sum.AttachmentExts, []string{"pdf", "txt"}) {
		t.Errorf("attachments = %v %v", sum.HasAttachment, sum.AttachmentExts)
	}
	if sum.From != "priya@example.org" || sum.Subject != "Signed contract attached" ||
		!strings.HasPrefix(sum.Body, "Hi,\n\nThe signed contract is attached.") || strings.Contains(sum.Body, "NOT THE BODY") {
		t.Errorf("summary = %+v", sum)
	}
	if sum.ReceivedAt.IsZero() || sum.SizeKB == 0 {
		t.Errorf("received %v, size %v", sum.ReceivedAt, sum.SizeKB)
	}
}

func TestToFoldersArchivesToAllMail(t *testing.T) {
	gmail, _ := presets.Get("gmail")
	attrs := func(a ...imap.MailboxAttr) []imap.MailboxAttr { return a }
	for _, tc := range []struct {
		name string
		list []*imap.ListData
		want map[string]string // folder -> role, only folders that get one
	}{
		{"\\All serves as Archive when there is no \\Archive", []*imap.ListData{
			{Mailbox: "INBOX"},
			{Mailbox: "[Gmail]", Attrs: attrs(imap.MailboxAttrNoSelect)},
			{Mailbox: "[Gmail]/All Mail", Attrs: attrs(imap.MailboxAttrHasNoChildren, imap.MailboxAttrAll)},
			{Mailbox: "[Gmail]/Trash", Attrs: attrs(imap.MailboxAttrTrash)},
		}, map[string]string{"[Gmail]/All Mail": mail.RoleArchive, "[Gmail]/Trash": mail.RoleTrash}},
		{"\\Archive wins over \\All", []*imap.ListData{
			{Mailbox: "All Mail", Attrs: attrs(imap.MailboxAttrAll)},
			{Mailbox: "Old", Attrs: attrs(imap.MailboxAttrArchive)},
		}, map[string]string{"Old": mail.RoleArchive}},
		{"\\All wins over a folder that is only named Archive", []*imap.ListData{
			{Mailbox: "Archive"},
			{Mailbox: "All Mail", Attrs: attrs(imap.MailboxAttrAll)},
		}, map[string]string{"All Mail": mail.RoleArchive}},
		{"a folder named Archive still counts without \\All", []*imap.ListData{
			{Mailbox: "Archive"},
		}, map[string]string{"Archive": mail.RoleArchive}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, f := range toFolders(tc.list, gmail) {
				if f.SpecialUse != "" {
					got[f.Name] = f.SpecialUse
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("roles = %v, want %v", got, tc.want)
			}
		})
	}
}
