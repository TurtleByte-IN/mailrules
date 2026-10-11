package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/unsubscribe"
)

// senderWindow is how far back the Senders screen counts mail, in seconds.
const senderWindow = 30 * 24 * 3600

// senderJSON is one sender: an address mail came from, or a domain that has a sender rule,
// with how it is routed (the contract's Sender).
type senderJSON struct {
	Type               string `json:"type"` // address | domain
	Value              string `json:"value"`
	Name               string `json:"name"`
	Messages           int    `json:"messages"`
	LastSeenAt         *int64 `json:"last_seen_at"`
	HasListUnsubscribe bool   `json:"has_list_unsubscribe"`
	// OneClick: the address's mail offers a one-click unsubscribe link MailRules can call.
	OneClick       bool    `json:"one_click_unsubscribe"`
	UnsubscribedAt *int64  `json:"unsubscribed_at"`
	Verdict        *string `json:"verdict"` // null = no sender rule; the rules decide
	RuleID         *int64  `json:"rule_id"`
	Folder         *string `json:"folder"` // for move: where the sender's mail goes
	Source         *string `json:"source"`
	Hits           int     `json:"hits"`
}

// senders merges what was seen in the last 30 days with the sender rules: every address
// that sent mail, every address with a rule even if it has been quiet, and every domain
// with a rule, counted over its addresses.
// ponytail: built in memory on every call; one user's 30 days are a few thousand
// addresses. Page in SQL if the Senders screen gets slow.
func (s *server) senders(ctx context.Context, v store.Viewer) ([]senderJSON, error) {
	seen, err := s.store.SendersSeen(ctx, v, s.now().Unix()-senderWindow)
	if err != nil {
		return nil, err
	}
	srs, err := s.store.SenderRules(ctx, v.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]senderJSON, 0, len(seen)+len(srs))
	byAddr := map[string]int{}
	for _, v := range seen {
		byAddr[v.Address] = len(out)
		out = append(out, senderJSON{Type: rules.MatchAddress, Value: v.Address, Name: v.Name, Messages: v.Messages,
			LastSeenAt: ts(v.LastSeenAt), HasListUnsubscribe: v.ListUnsubscribe, OneClick: v.OneClick, UnsubscribedAt: ts(v.UnsubscribedAt)})
	}
	for _, sr := range srs {
		i, ok := byAddr[sr.Value]
		if sr.MatchType == rules.MatchDomain || !ok {
			i = len(out)
			row := senderJSON{Type: sr.MatchType, Value: sr.Value}
			var last int64
			for _, v := range seen { // a domain rule counts the mail of all its addresses
				if _, domain, _ := strings.Cut(v.Address, "@"); sr.MatchType == rules.MatchDomain && (domain == sr.Value || strings.HasSuffix(domain, "."+sr.Value)) {
					row.Messages += v.Messages
					row.HasListUnsubscribe = row.HasListUnsubscribe || v.ListUnsubscribe
					last = max(last, v.LastSeenAt)
				}
			}
			row.LastSeenAt = ts(last)
			out = append(out, row)
		}
		out[i].Verdict, out[i].RuleID, out[i].Source, out[i].Hits = &sr.Verdict, ts(sr.RuleID), &sr.Source, sr.Hits
		if sr.Verdict == rules.VerdictMove {
			out[i].Folder = &sr.Folder
		}
	}
	return out, nil
}

// handleSenders lists senders by volume (or by when they last wrote), a page at a time.
func (s *server) handleSenders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sort, source, find := cmp.Or(q.Get("sort"), "volume"), q.Get("source"), strings.ToLower(strings.TrimSpace(q.Get("q")))
	if sort != "volume" && sort != "recent" {
		invalid(w, "sort", "Sort by volume or recent.")
		return
	}
	if source != "" && source != "user" && source != "learned" {
		invalid(w, "source", "The source is user or learned.")
		return
	}
	offset, limit := 0, 50
	for name, dst := range map[string]*int{"cursor": &offset, "limit": &limit} {
		if v := q.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || (name == "limit" && (n < 1 || n > 100)) {
				invalid(w, name, "The "+name+" is not valid.")
				return
			}
			*dst = n
		}
	}
	all, err := s.senders(r.Context(), viewer(r))
	if err != nil {
		internalError(w, r, err)
		return
	}
	all = slices.DeleteFunc(all, func(x senderJSON) bool {
		return (source != "" && (x.Source == nil || *x.Source != source)) ||
			(find != "" && !strings.Contains(x.Value, find) && !strings.Contains(strings.ToLower(x.Name), find))
	})
	last := func(x senderJSON) int64 {
		if x.LastSeenAt == nil {
			return 0
		}
		return *x.LastSeenAt
	}
	slices.SortFunc(all, func(a, b senderJSON) int {
		if sort == "recent" {
			return cmp.Or(cmp.Compare(last(b), last(a)), cmp.Compare(a.Value, b.Value))
		}
		return cmp.Or(cmp.Compare(b.Messages, a.Messages), cmp.Compare(a.Value, b.Value))
	})
	var next *string
	page := all[min(offset, len(all)):]
	if len(page) > limit {
		page = page[:limit]
		c := strconv.Itoa(offset + limit)
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page, "next_cursor": next})
}

// senderPath reads the {type}/{value} of a sender route: an address or a domain, lower-case.
func senderPath(w http.ResponseWriter, r *http.Request) (matchType, value string, ok bool) {
	matchType, value = r.PathValue("type"), strings.ToLower(strings.TrimSpace(r.PathValue("value")))
	local, domain, isAddr := strings.Cut(value, "@")
	switch {
	case matchType != rules.MatchAddress && matchType != rules.MatchDomain:
		invalid(w, "type", "The sender type is address or domain.")
	case strings.ContainsAny(value, " \t/") || (matchType == rules.MatchAddress && (!isAddr || local == "" || !strings.Contains(domain, "."))) ||
		(matchType == rules.MatchDomain && (isAddr || !strings.Contains(value, "."))):
		invalid(w, "value", "Give the sender as an address (name@example.com) or a domain (example.com).")
	default:
		return matchType, value, true
	}
	return "", "", false
}

// handleSenderPut sets how a sender is routed: to a rule, to a folder, always kept in the
// inbox, or blocked. It replaces whatever the sender had, a learned rule included.
func (s *server) handleSenderPut(w http.ResponseWriter, r *http.Request) {
	matchType, value, ok := senderPath(w, r)
	if !ok {
		return
	}
	var in struct {
		Verdict string `json:"verdict"`
		RuleID  *int64 `json:"rule_id"`
		Folder  string `json:"folder"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	sr := rules.SenderRule{UserID: user(r).ID, MatchType: matchType, Value: value, Verdict: in.Verdict, Source: "user"}
	switch in.Verdict {
	case rules.VerdictKeep, rules.VerdictBlock:
	case rules.VerdictRoute:
		if in.RuleID == nil {
			invalid(w, "rule_id", "Say which rule the sender's mail goes to.")
			return
		}
		if _, err := s.store.Rule(r.Context(), viewer(r).TenantID, *in.RuleID); err != nil {
			invalid(w, "rule_id", "No such rule.")
			return
		}
		sr.RuleID = *in.RuleID
	case rules.VerdictMove:
		// The name is used on every mailbox, as a rule's move is: one that lacks the folder
		// gets it on the first live move.
		if in.Folder == "" {
			invalid(w, "folder", "Say which folder the sender's mail goes to.")
			return
		}
		if msg := rules.FolderProblem(in.Folder); msg != "" {
			invalid(w, "folder", msg)
			return
		}
		sr.Folder = in.Folder
	default:
		invalid(w, "verdict", "The verdict is route, move, keep or block.")
		return
	}
	if _, err := s.store.PutSenderRule(r.Context(), viewer(r).TenantID, sr, s.now().Unix()); err != nil {
		internalError(w, r, err)
		return
	}
	all, err := s.senders(r.Context(), viewer(r))
	if err != nil {
		internalError(w, r, err)
		return
	}
	i := slices.IndexFunc(all, func(x senderJSON) bool { return x.Type == matchType && x.Value == value })
	writeJSON(w, http.StatusOK, map[string]any{"sender": all[i]})
}

// handleSenderDelete removes a sender rule, user-made or learned; the rules decide again.
func (s *server) handleSenderDelete(w http.ResponseWriter, r *http.Request) {
	matchType, value, ok := senderPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSenderRule(r.Context(), viewer(r).TenantID, matchType, value); err != nil {
		internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSenderUnsubscribe unsubscribes from an address with the one-click link (RFC 8058)
// of its newest email, on every mailbox the person sees that is not unsubscribed from it
// yet. It is a click of the person's, not automation, and changes no mailbox, so dry-run
// does not hold it back. Once every mailbox is unsubscribed, an address without a sender
// rule (of its own or of its domain) gets one that trashes what still comes: the
// stragglers a list sends while the unsubscribe takes effect.
func (s *server) handleSenderUnsubscribe(w http.ResponseWriter, r *http.Request) {
	matchType, value, ok := senderPath(w, r)
	if !ok {
		return
	}
	if matchType != rules.MatchAddress {
		invalid(w, "type", "Unsubscribe from one address at a time: a domain has no unsubscribe link of its own.")
		return
	}
	ctx, v := r.Context(), viewer(r)
	links, err := s.store.UnsubscribeLinks(ctx, v, value, s.now().Unix()-senderWindow)
	if err != nil {
		internalError(w, r, err)
		return
	}
	done, failure := 0, ""
	for _, l := range links {
		host := unsubscribe.Host(l.URL) // never the link: it often carries a token naming the person
		if err := s.Unsubscriber.Unsubscribe(ctx, l.URL); err != nil {
			slog.InfoContext(ctx, "unsubscribe failed", "account_id", l.AccountID, "host", host, "error", err.Error())
			failure = cmp.Or(failure, unsubscribeFailure(host, err))
			continue
		}
		if err := s.store.AddUnsubscribe(ctx, l.AccountID, value, host, s.now().Unix()); err != nil {
			internalError(w, r, err)
			return
		}
		slog.InfoContext(ctx, "unsubscribed", "account_id", l.AccountID, "host", host)
		done++
	}
	if failure != "" {
		writeError(w, http.StatusBadGateway, "unsubscribe_failed", failure, "")
		return
	}
	sender, err := s.sender(ctx, v, value)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if sender == nil || !sender.OneClick {
		writeError(w, http.StatusConflict, "no_unsubscribe_link",
			"This sender's mail has no one-click unsubscribe link, so MailRules cannot unsubscribe you. Choose Always trash instead.", "")
		return
	}
	if done > 0 && sender.Verdict == nil {
		srs, err := s.store.SenderRules(ctx, v.TenantID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		if !slices.ContainsFunc(srs, func(sr rules.SenderRule) bool { return sr.MatchType == rules.MatchDomain && inDomain(value, sr.Value) }) {
			sr := rules.SenderRule{UserID: user(r).ID, MatchType: rules.MatchAddress, Value: value, Verdict: rules.VerdictBlock, Source: "user"}
			if _, err := s.store.PutSenderRule(ctx, v.TenantID, sr, s.now().Unix()); err != nil {
				internalError(w, r, err)
				return
			}
			if sender, err = s.sender(ctx, v, value); err != nil {
				internalError(w, r, err)
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sender": sender})
}

// sender is the Senders row of one address, or nil when it has none.
func (s *server) sender(ctx context.Context, v store.Viewer, address string) (*senderJSON, error) {
	all, err := s.senders(ctx, v)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(all, func(x senderJSON) bool { return x.Type == rules.MatchAddress && x.Value == address }); i >= 0 {
		return &all[i], nil
	}
	return nil, nil
}

// inDomain reports whether address is at domain or one of its subdomains, as a domain
// sender rule matches it.
func inDomain(address, domain string) bool {
	_, d, _ := strings.Cut(address, "@")
	return d == domain || strings.HasSuffix(d, "."+domain)
}

// unsubscribeFailure is what the person is told when a site did not unsubscribe them.
func unsubscribeFailure(host string, err error) string {
	var refused *unsubscribe.RefusedError
	switch {
	case errors.Is(err, mail.ErrNotPublic):
		return "The unsubscribe link of " + host + " leads to a private network, so MailRules did not open it. Choose Always trash instead."
	case errors.As(err, &refused):
		return host + " answered " + strconv.Itoa(refused.Status) + " and did not confirm, so you may still be subscribed. Try again later, or choose Always trash."
	default:
		return host + " did not answer, so you are still subscribed. Try again later, or choose Always trash."
	}
}
