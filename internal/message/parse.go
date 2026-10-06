package message

import (
	"bytes"
	"fmt"
	"io"
	"net/textproto"
	"strings"
	"unicode"
	"unicode/utf8"

	gomessage "github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decodes non-UTF-8 charsets
	gomail "github.com/emersion/go-message/mail"
	"github.com/k3a/html2text"
)

// maxPart bounds how much of one MIME part is decoded; Raw.Text is already capped at 64 KB.
const maxPart = 256 * 1024

// Parse turns a fetched message into the Summary that rules and models see. The body is
// plain text (HTML is converted), stripped of control and invisible characters and cut to
// bodyChars characters; zero or less keeps it whole. IsContact and RepliedBefore are left
// false: the caller fills them from the contacts index.
//
// Raw.Text may stop mid-message, so a body that ends early is used as far as it goes.
func Parse(raw *Raw, accountID int64, bodyChars int) (*Summary, error) {
	entity, err := gomessage.Read(io.MultiReader(bytes.NewReader(raw.Header), bytes.NewReader(raw.Text)))
	if err != nil && !gomessage.IsUnknownCharset(err) && !gomessage.IsUnknownEncoding(err) {
		return nil, fmt.Errorf("parse message header: %w", err)
	}
	h := gomail.Header{Header: entity.Header}

	s := &Summary{
		AccountID:      accountID,
		To:             addresses(h, "To"),
		Cc:             addresses(h, "Cc"),
		DeliveredTo:    addresses(h, "Delivered-To"),
		Headers:        map[string][]string{},
		HasAttachment:  raw.HasAttachment,
		AttachmentExts: raw.AttachmentExts,
		SizeKB:         float64(raw.Size) / 1024,
		ReceivedAt:     raw.InternalDate,
	}
	subject, _ := h.Subject() // on a decoding error the raw value comes back, which is still useful
	s.Subject = oneLine(subject)
	if from, _ := h.AddressList("From"); len(from) > 0 {
		s.From = strings.ToLower(from[0].Address)
		s.FromName = oneLine(from[0].Name)
		if at := strings.LastIndexByte(s.From, '@'); at >= 0 {
			s.FromDomain = s.From[at+1:]
		}
	}
	for f := entity.Header.Fields(); f.Next(); {
		v, err := f.Text()
		if err != nil {
			v = f.Value()
		}
		key := textproto.CanonicalMIMEHeaderKey(f.Key())
		s.Headers[key] = append(s.Headers[key], oneLine(v))
	}

	s.ListID = listID(first(s.Headers, "List-Id"))
	precedence := strings.ToLower(strings.TrimSpace(first(s.Headers, "Precedence")))
	s.IsBulk = len(s.Headers["List-Unsubscribe"]) > 0 || precedence == "bulk" || precedence == "list"
	s.IsNoreply = isNoreply(s.From)
	s.DMARC = dmarc(s.Headers["Authentication-Results"])

	plain, html := bodyText(entity)
	if strings.TrimSpace(plain) == "" && html != "" {
		plain = html2text.HTML2TextWithOptions(html, html2text.WithUnixLineBreaks(), html2text.WithLinksInnerText())
	}
	s.Body = truncate(Clean(plain), bodyChars)
	return s, nil
}

func first(h map[string][]string, key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// addresses returns the lower-cased addresses of every field with that key.
func addresses(h gomail.Header, key string) []string {
	var out []string
	for f := h.FieldsByKey(key); f.Next(); {
		list, err := gomail.ParseAddressList(f.Value())
		if err != nil {
			continue
		}
		for _, a := range list {
			out = append(out, strings.ToLower(a.Address))
		}
	}
	return out
}

// bodyText returns the first text/plain and first text/html part that is not an attachment.
func bodyText(e *gomessage.Entity) (plain, html string) {
	mediaType, _, _ := e.Header.ContentType()
	if mr := e.MultipartReader(); mr != nil {
		for plain == "" {
			part, err := mr.NextPart()
			if err != nil && !gomessage.IsUnknownCharset(err) && !gomessage.IsUnknownEncoding(err) {
				break // end of message, or the fetched text stopped here
			}
			p, h := bodyText(part)
			if plain == "" {
				plain = p
			}
			if html == "" {
				html = h
			}
		}
		return plain, html
	}
	if disp, _, _ := e.Header.ContentDisposition(); strings.EqualFold(disp, "attachment") {
		return "", ""
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		return "", ""
	}
	b, _ := io.ReadAll(io.LimitReader(e.Body, maxPart)) // a cut-off encoding still yields its start
	text := strings.ToValidUTF8(string(b), "")
	if mediaType == "text/html" {
		return "", text
	}
	return text, ""
}

// Clean makes untrusted text safe to show a model: it drops control and invisible
// formatting characters (zero-width spaces and joiners, bidi overrides, BOMs, soft
// hyphens), turns every kind of space into a plain one and collapses blank runs.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t' || unicode.Is(unicode.Zs, r):
			return ' '
		case r == utf8.RuneError || r == '͏' || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Zl, unicode.Zp):
			return -1
		}
		return r
	}, s)
	var b strings.Builder
	blank := true // also trims leading blank lines
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				b.WriteByte('\n')
			}
			blank = true
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		blank = false
	}
	return strings.TrimSpace(b.String())
}

// oneLine cleans a header value that must stay on one line.
func oneLine(s string) string { return strings.Join(strings.Fields(Clean(s)), " ") }

// truncate cuts s to at most n characters; n <= 0 keeps everything.
func truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// listID pulls the identifier out of `Some list <id.example.com>`.
func listID(v string) string {
	if open := strings.LastIndexByte(v, '<'); open >= 0 {
		if end := strings.IndexByte(v[open:], '>'); end > 0 {
			v = v[open+1 : open+end]
		}
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// isNoreply matches noreply@, no-reply@, do_not_reply@, noreply+tag@ and the like.
func isNoreply(addr string) bool {
	local, _, _ := strings.Cut(addr, "@")
	local = strings.NewReplacer("-", "", "_", "", ".", "").Replace(local)
	return strings.Contains(local, "noreply") || strings.Contains(local, "donotreply")
}

// dmarc reads the verdict from the topmost Authentication-Results header that has one;
// receiving servers add theirs above anything the sender wrote.
// ponytail: this trusts header order rather than checking the authserv-id, so a server
// that records no DMARC result lets a forged header through. Pin the authserv-id per
// account if this signal ever gates an action on its own.
func dmarc(results []string) string {
	for _, r := range results {
		_, rest, ok := strings.Cut(strings.ToLower(r), "dmarc=")
		if !ok {
			continue
		}
		switch verdict, _, _ := strings.Cut(rest, " "); strings.TrimRight(strings.TrimSpace(verdict), ";") {
		case "pass":
			return "pass"
		case "fail":
			return "fail"
		}
		return "none"
	}
	return "none"
}
