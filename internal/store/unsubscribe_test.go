package store

import (
	"slices"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// The one-click link of each mailbox's newest email from an address is the one called; a
// mailbox once unsubscribed is not called again, and the sender shows as unsubscribed only
// when every mailbox the viewer sees is, which survives a reload because it is stored.
func TestUnsubscribeLinksAndState(t *testing.T) {
	w := twoTenants(t)
	ctx := t.Context()
	uid := uint32(0)
	put := func(a Account, from, link string, at int64) {
		t.Helper()
		uid++
		m, _, err := w.s.IngestMessage(ctx, mail.MsgRef{AccountID: a.ID, Folder: "INBOX", UIDValidity: 1, UID: uid}, at)
		if err != nil {
			t.Fatal(err)
		}
		m.FromAddr, m.FromDomain, m.UnsubscribeURL, m.ReceivedAt = from, "news.example", link, at
		if err := w.s.SaveMessageSummary(ctx, m); err != nil {
			t.Fatal(err)
		}
		if got, err := w.s.Message(ctx, m.ID); err != nil || got.UnsubscribeURL != link {
			t.Fatalf("message = %+v, %v", got, err)
		}
	}
	const news = "list@news.example"
	put(w.aPriv, news, "https://news.example/u/priv-old", 100)
	put(w.aPriv, news, "https://news.example/u/priv-new", 200)
	put(w.aShared, news, "https://news.example/u/shared", 150)
	put(w.aShared, news, "", 300) // the newest offers no link: the one before it is used
	put(w.aPriv, "plain@news.example", "", 300)
	put(w.bAcct, news, "https://news.example/u/b", 100)

	links := func(u User) []UnsubscribeLink {
		t.Helper()
		got, err := w.s.UnsubscribeLinks(ctx, u.Viewer(), news, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	seen := func(u User, addr string) SenderSeen {
		t.Helper()
		all, err := w.s.SendersSeen(ctx, u.Viewer(), 0)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(all, func(s SenderSeen) bool { return s.Address == addr })
		if i < 0 {
			t.Fatalf("%s not seen by %s", addr, u.Email)
		}
		return all[i]
	}

	if got := links(w.aUser); !slices.Equal(got, []UnsubscribeLink{{w.aPriv.ID, "https://news.example/u/priv-new"}, {w.aShared.ID, "https://news.example/u/shared"}}) {
		t.Errorf("owner's links = %v", got)
	}
	if got := links(w.mate); !slices.Equal(got, []UnsubscribeLink{{w.aShared.ID, "https://news.example/u/shared"}}) {
		t.Errorf("teammate's links = %v", got)
	}
	if got := links(w.bUser); !slices.Equal(got, []UnsubscribeLink{{w.bAcct.ID, "https://news.example/u/b"}}) {
		t.Errorf("other tenant's links = %v", got)
	}
	if got, err := w.s.UnsubscribeLinks(ctx, w.aUser.Viewer(), news, 250); err != nil || len(got) != 0 {
		t.Errorf("links of mail seen since 250 = %v, %v", got, err)
	}
	if s := seen(w.aUser, news); !s.OneClick || s.UnsubscribedAt != 0 || seen(w.aUser, "plain@news.example").OneClick {
		t.Errorf("before = %+v", s)
	}

	// One mailbox done: the other is still to do.
	if err := w.s.AddUnsubscribe(ctx, w.aPriv.ID, news, "news.example", 500); err != nil {
		t.Fatal(err)
	}
	if got := links(w.aUser); len(got) != 1 || got[0].AccountID != w.aShared.ID {
		t.Errorf("owner's links after one = %v", got)
	}
	if s := seen(w.aUser, news); !s.OneClick || s.UnsubscribedAt != 0 {
		t.Errorf("half done = %+v", s)
	}
	// Both done: unsubscribed for everyone who sees those mailboxes, and no other tenant.
	if err := w.s.AddUnsubscribe(ctx, w.aShared.ID, news, "news.example", 600); err != nil {
		t.Fatal(err)
	}
	if got := links(w.aUser); len(got) != 0 {
		t.Errorf("owner's links after both = %v", got)
	}
	for _, u := range []User{w.aUser, w.mate} {
		if s := seen(u, news); !s.OneClick || s.UnsubscribedAt != 600 {
			t.Errorf("%s sees %+v", u.Email, s)
		}
	}
	if s := seen(w.bUser, news); s.UnsubscribedAt != 0 || len(links(w.bUser)) != 1 {
		t.Errorf("the other tenant sees %+v", s)
	}
	// Recording it again moves the time, and is not a second row.
	if err := w.s.AddUnsubscribe(ctx, w.aShared.ID, news, "news.example", 700); err != nil {
		t.Fatal(err)
	}
	if s := seen(w.aUser, news); s.UnsubscribedAt != 700 {
		t.Errorf("after recording again = %+v", s)
	}
}
