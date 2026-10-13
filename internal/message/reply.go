package message

import (
	"bytes"
	"mime"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"time"
)

// maxReferences is how many Message-IDs a reply's References keeps: the thread's newest.
const maxReferences = 20

// Reply builds a plain-text reply to orig, from from, saying body: the message a draft reply
// is saved as. To is orig's Reply-To, else its sender; Subject is orig's with "Re: " in front
// unless it has one; In-Reply-To and References thread it under orig (none when orig has no
// Message-ID). Its own Message-ID is made from key, so the same key always gives the same
// one, at from's domain. It returns the message, with CRLF line ends, and that Message-ID
// without angle brackets. No header value can carry a line break: a value that has one is
// cut there.
func Reply(orig *Summary, from netmail.Address, body, key string, now time.Time) (raw []byte, messageID string) {
	domain := "mailrules.invalid"
	if at := strings.LastIndexByte(from.Address, '@'); at >= 0 && at < len(from.Address)-1 {
		domain = from.Address[at+1:]
	}
	messageID = "mailrules-draft-" + key + "@" + domain

	var b bytes.Buffer
	header := func(name, value string) {
		if i := strings.IndexAny(value, "\r\n"); i >= 0 {
			value = value[:i]
		}
		b.WriteString(name + ": " + value + "\r\n")
	}
	header("Date", now.Format(time.RFC1123Z))
	header("From", from.String())
	if to := replyTo(orig); len(to) > 0 {
		list := make([]string, len(to))
		for i, a := range to {
			list[i] = a.String()
		}
		header("To", strings.Join(list, ", "))
	}
	subject := oneLine(orig.Subject)
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = strings.TrimSpace("Re: " + subject)
	}
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Message-ID", "<"+messageID+">")
	if parent := msgID(first(orig.Headers, "Message-Id")); parent != "" {
		header("In-Reply-To", "<"+parent+">")
		refs := msgIDs(first(orig.Headers, "References"))
		if len(refs) == 0 {
			refs = msgIDs(first(orig.Headers, "In-Reply-To"))
		}
		refs = append(refs, "<"+parent+">")
		if len(refs) > maxReferences {
			refs = refs[len(refs)-maxReferences:]
		}
		// One ID per line, so the field never passes the 998-character line limit.
		b.WriteString("References: " + strings.Join(refs, "\r\n ") + "\r\n")
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(body, "\r\n", "\n"))) // a bytes.Buffer does not fail
	_ = qp.Close()
	return b.Bytes(), messageID
}

// replyTo is who a reply to s goes to: its Reply-To when that reads as addresses, else its
// sender.
func replyTo(s *Summary) []*netmail.Address {
	if list, err := netmail.ParseAddressList(first(s.Headers, "Reply-To")); err == nil && len(list) > 0 {
		return list
	}
	if s.From == "" {
		return nil
	}
	return []*netmail.Address{{Name: s.FromName, Address: s.From}}
}

// msgID is one Message-ID as a header has it, without angle brackets; "" when it is not one.
func msgID(v string) string {
	ids := msgIDs(v)
	if len(ids) == 0 {
		return ""
	}
	return strings.Trim(ids[0], "<>")
}

// msgIDs are the <...> Message-IDs in a References or In-Reply-To value, brackets kept.
func msgIDs(v string) []string {
	var out []string
	for _, f := range strings.Fields(v) {
		if strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") && len(f) > 2 && !strings.ContainsAny(f[1:len(f)-1], "<>") {
			out = append(out, f)
		}
	}
	if len(out) == 0 { // a bare ID, as some mailers write Message-ID
		if f := strings.TrimSpace(v); f != "" && !strings.ContainsAny(f, " <>") && strings.Contains(f, "@") {
			out = append(out, "<"+f+">")
		}
	}
	return out
}
