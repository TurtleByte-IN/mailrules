package message

import (
	"bytes"
	"io"
	"mime"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"testing"
	"time"
)

// A reply threads under the email it answers, goes to whoever the email asks replies to go
// to, and carries the text it was given and nothing else.
func TestReply(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)
	from := netmail.Address{Address: "me@icloud.com"}
	tests := []struct {
		name                         string
		header                       string
		wantTo, wantSubject          string
		wantInReplyTo, wantReference string
	}{
		{name: "a first email", header: "From: Asha Rao <asha@example.com>\r\nSubject: Coffee next week?\r\nMessage-ID: <a1@example.com>\r\n",
			wantTo: `"Asha Rao" <asha@example.com>`, wantSubject: "Re: Coffee next week?", wantInReplyTo: "<a1@example.com>", wantReference: "<a1@example.com>"},
		{name: "a reply keeps its thread", header: "From: asha@example.com\r\nSubject: RE: Coffee\r\nMessage-ID: <a3@example.com>\r\n" +
			"In-Reply-To: <a2@example.com>\r\nReferences: <a1@example.com> <a2@example.com>\r\n",
			wantTo: "<asha@example.com>", wantSubject: "RE: Coffee", wantInReplyTo: "<a3@example.com>",
			wantReference: "<a1@example.com>\r\n <a2@example.com>\r\n <a3@example.com>"},
		{name: "Reply-To wins over From", header: "From: news@shop.example\r\nReply-To: Help <help@shop.example>\r\nSubject: Order\r\nMessage-ID: <o1@shop.example>\r\n",
			wantTo: `"Help" <help@shop.example>`, wantSubject: "Re: Order", wantInReplyTo: "<o1@shop.example>", wantReference: "<o1@shop.example>"},
		{name: "no Message-ID: no thread", header: "From: a@example.com\r\nSubject: Hi\r\n", wantTo: "<a@example.com>", wantSubject: "Re: Hi"},
		{name: "a subject that needs encoding", header: "From: a@example.com\r\nSubject: =?utf-8?q?Caf=C3=A9?=\r\nMessage-ID: <c@example.com>\r\n",
			wantTo: "<a@example.com>", wantSubject: "Re: Café", wantInReplyTo: "<c@example.com>", wantReference: "<c@example.com>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, err := Parse(&Raw{Header: []byte(tt.header + "\r\n"), Text: []byte("hello")}, 1, 2000)
			if err != nil {
				t.Fatal(err)
			}
			raw, id := Reply(orig, from, "Thanks, that works.\nSee you then.", "KEY1", now)
			if id != "mailrules-draft-KEY1@icloud.com" {
				t.Errorf("Message-ID = %q", id)
			}
			again, _ := Reply(orig, from, "Thanks, that works.\nSee you then.", "KEY1", now)
			if !bytes.Equal(raw, again) {
				t.Error("the same key gave another message")
			}
			msg, err := netmail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			h := msg.Header
			subject, _ := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
			if h.Get("From") != "<me@icloud.com>" || h.Get("To") != tt.wantTo || subject != tt.wantSubject ||
				h.Get("Message-Id") != "<"+id+">" || h.Get("In-Reply-To") != tt.wantInReplyTo || h.Get("Date") != "Tue, 13 Oct 2026 09:30:00 +0000" {
				t.Errorf("headers = %v (subject %q)", h, subject)
			}
			if got := headerField(raw, "References"); got != tt.wantReference {
				t.Errorf("References = %q, want %q", got, tt.wantReference)
			}
			body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
			if string(body) != "Thanks, that works.\r\nSee you then." {
				t.Errorf("body = %q", body)
			}
		})
	}
}

// No header of a reply can be split by what the email it answers says.
func TestReplyHeadersStayOnOneLine(t *testing.T) {
	orig := &Summary{From: "a@example.com", Subject: "Hi\r\nBcc: victim@example.com", Headers: map[string][]string{"Message-Id": {"<x@example.com>"}}}
	raw, _ := Reply(orig, netmail.Address{Address: "me@example.test"}, "ok", "K", time.Unix(0, 0))
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if msg.Header.Get("Bcc") != "" || strings.ContainsAny(subject, "\r\n") {
		t.Errorf("a header was injected:\n%s", raw)
	}
}

// headerField is one field of a message's header as written, folded lines included.
func headerField(raw []byte, name string) string {
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	for i, l := range lines {
		if v, ok := strings.CutPrefix(l, name+": "); ok {
			for _, more := range lines[i+1:] {
				if !strings.HasPrefix(more, " ") {
					break
				}
				v += "\r\n" + more
			}
			return v
		}
	}
	return ""
}
