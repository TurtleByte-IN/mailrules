package mailtest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

const eml = "From: a@example.org\nMessage-ID: <one@example.org>\nSubject: hi\n\nhello world\n"

func TestFakeKeepsTheConnectorsPromises(t *testing.T) {
	ctx := t.Context()
	m := New(3)
	m.AddFolder("Food", "")
	first := m.Deliver("INBOX", eml)

	// Watch: backlog first, then deliveries, in order.
	wctx, cancel := context.WithCancel(ctx)
	out := make(chan mail.NewMail)
	done := make(chan error, 1)
	go func() { done <- m.Watch(wctx, "INBOX", 0, out) }()
	next := func() mail.MsgRef {
		t.Helper()
		select {
		case nm := <-out:
			return nm.Ref
		case <-time.After(5 * time.Second):
			t.Fatal("no mail reported")
			return mail.MsgRef{}
		}
	}
	if got := next(); got != first {
		t.Fatalf("catch-up = %+v, want %+v", got, first)
	}
	second := m.Deliver("INBOX", eml)
	if got := next(); got != second || second.UID != 2 {
		t.Fatalf("arrival = %+v, want %+v", got, second)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Watch returned %v", err)
	}

	// Fetch leaves the message unread and honours maxBody.
	raw, err := m.Fetch(ctx, first, 5)
	if err != nil || string(raw.Text) != "hello" || len(raw.Header) == 0 {
		t.Fatalf("fetch = %+v, %v", raw, err)
	}
	if flags, _ := m.Flags(ctx, first); len(flags) != 0 {
		t.Errorf("flags after fetch = %v", flags)
	}
	if err := m.SetFlags(ctx, first, []string{`\Seen`, `\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFlags(ctx, first, nil, []string{`\Seen`}); err != nil {
		t.Fatal(err)
	}
	if flags, _ := m.Flags(ctx, first); !slices.Equal(flags, []string{`\Flagged`}) {
		t.Errorf("flags = %v", flags)
	}

	// Move: new UID in the destination, gone from the source, findable by Message-ID.
	moved, err := m.Move(ctx, first, "Food")
	if err != nil || moved.Folder != "Food" || moved.UID != 1 || moved.UIDValidity == first.UIDValidity {
		t.Fatalf("moved = %+v, %v", moved, err)
	}
	if _, err := m.Fetch(ctx, first, 0); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("fetch of a moved message: %v", err)
	}
	if got, err := m.FindByMessageID(ctx, "Food", "one@example.org"); err != nil || got != moved {
		t.Errorf("find = %+v, %v", got, err)
	}
	if _, err := m.Move(ctx, moved, "Nowhere"); !errors.Is(err, mail.ErrNoFolder) {
		t.Errorf("move to a missing folder: %v", err)
	}
	stale := moved
	stale.UIDValidity++
	if _, err := m.Flags(ctx, stale); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("stale uidvalidity: %v", err)
	}

	if st, err := m.Status(ctx, "INBOX"); err != nil || st.UIDNext != 3 || st.UIDValidity != second.UIDValidity {
		t.Errorf("status = %+v, %v", st, err)
	}
	if refs, err := m.FetchSince(ctx, "INBOX", time.Now().Add(-time.Hour), 0); err != nil || len(refs) != 1 || refs[0] != second {
		t.Errorf("FetchSince = %+v, %v", refs, err)
	}
	if err := m.EnsureFolder(ctx, "New"); err != nil {
		t.Fatal(err)
	}
	if folders, _ := m.Folders(ctx); len(folders) != 3 || folders[0].Name != "Food" {
		t.Errorf("folders = %+v", folders)
	}

	m.Caps = mail.Caps{}
	if _, err := m.Move(ctx, second, "Food"); !errors.Is(err, mail.ErrUnsupported) {
		t.Errorf("move on a limited server: %v", err)
	}
}
