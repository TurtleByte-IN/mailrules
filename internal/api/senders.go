package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/rules"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// senderWindow is how far back the Senders screen counts mail, in seconds.
const senderWindow = 30 * 24 * 3600

// senderJSON is one sender: an address mail came from, or a domain that has a sender rule,
// with how it is routed (the contract's Sender).
type senderJSON struct {
	Type               string  `json:"type"` // address | domain
	Value              string  `json:"value"`
	Name               string  `json:"name"`
	Messages           int     `json:"messages"`
	LastSeenAt         *int64  `json:"last_seen_at"`
	HasListUnsubscribe bool    `json:"has_list_unsubscribe"`
	Verdict            *string `json:"verdict"` // null = no sender rule; the rules decide
	RuleID             *int64  `json:"rule_id"`
	Folder             *string `json:"folder"` // for move: where the sender's mail goes
	Source             *string `json:"source"`
	Hits               int     `json:"hits"`
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
			LastSeenAt: ts(v.LastSeenAt), HasListUnsubscribe: v.ListUnsubscribe})
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
