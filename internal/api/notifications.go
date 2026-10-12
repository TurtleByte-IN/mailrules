package api

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// notificationKinds are the destination kinds a module may deliver to.
var notificationKinds = []string{store.DestinationWebhook, store.DestinationSlack}

// recentDeliveries is how many deliveries Settings lists under each destination.
const recentDeliveries = 20

// notifier is the module that delivers rule notifications, or nil in a build without one.
func (s *server) notifier() *ext.Notifications {
	for _, m := range s.Modules {
		if m.Notifications != nil {
			return m.Notifications
		}
	}
	return nil
}

// notificationsOnly refuses a request while no module of the build delivers notifications.
func (s *server) notificationsOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.notifier() == nil {
			writeError(w, http.StatusNotFound, "not_available", "Notifications and webhooks are not included in this build of MailRules.", "")
			return
		}
		h(w, r)
	}
}

type deliveryJSON struct {
	At       int64  `json:"at"`
	RuleID   *int64 `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

type destinationJSON struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	Kind           string         `json:"kind"`
	URL            string         `json:"url"` // the address cut short
	AddressDeleted bool           `json:"address_deleted"`
	NeedsHTTPS     bool           `json:"needs_https"`
	CreatedAt      int64          `json:"created_at"`
	Deliveries     []deliveryJSON `json:"deliveries"`
}

func toDestinationJSON(d store.Destination, ds []store.Delivery) destinationJSON {
	out := destinationJSON{ID: d.ID, Name: d.Name, Kind: d.Kind, URL: d.URLHint, AddressDeleted: d.AddressDeleted, NeedsHTTPS: d.NeedsHTTPS(),
		CreatedAt: d.CreatedAt, Deliveries: make([]deliveryJSON, len(ds))}
	for i, x := range ds {
		out.Deliveries[i] = deliveryJSON{At: x.CreatedAt, RuleID: ts(x.RuleID), RuleName: x.RuleName, Status: x.Status, Reason: x.Reason}
	}
	return out
}

// handleNotifications lists the kinds the module delivers to and the tenant's destinations,
// each with its recent deliveries.
func (s *server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	tenant := viewer(r).TenantID
	ds, err := s.store.Destinations(r.Context(), tenant)
	if err != nil {
		internalError(w, r, err)
		return
	}
	recent, err := s.store.RecentDeliveries(r.Context(), tenant, recentDeliveries)
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := make([]destinationJSON, len(ds))
	for i, d := range ds {
		out[i] = toDestinationJSON(d, recent[d.ID])
	}
	writeJSON(w, http.StatusOK, map[string]any{"kinds": s.offeredKinds(), "destinations": out})
}

// offeredKinds are the destination kinds the module delivers to, in the contract's order.
func (s *server) offeredKinds() []string {
	kinds := []string{}
	for _, k := range notificationKinds {
		if slices.Contains(s.notifier().Kinds, k) {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// maxDestinationName and maxDestinationURL bound what a destination stores.
const (
	maxDestinationName = 100
	maxDestinationURL  = 2000
)

// destinationURLProblem is what is wrong with an address for a destination of kind, as a
// sentence; empty when it will do. Every destination takes https only.
func (s *server) destinationURLProblem(kind, raw string) string {
	u, err := url.Parse(raw)
	switch {
	case raw == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(raw) > maxDestinationURL:
		if kind == store.DestinationSlack {
			return "Enter the Slack incoming webhook address, which starts with https://hooks.slack.com/."
		}
		return "Enter the address as an https URL, such as https://example.com/mailrules."
	case u.User != nil:
		return "Leave the user name and password out of the address."
	case kind == store.DestinationSlack && (u.Scheme != "https" || u.Hostname() != "hooks.slack.com"):
		return "Enter the Slack incoming webhook address, which starts with https://hooks.slack.com/."
	case u.Scheme != "https":
		return "Use an https address, so the email's sender and subject are encrypted on the way."
	}
	return ""
}

// newWebhookSecret is a webhook's signing secret: 32 random bytes.
func newWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("make signing secret: %w", err)
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// handleDestinationCreate adds a destination of the tenant. A webhook gets a signing secret,
// answered this once.
func (s *server) handleDestinationCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name, raw := strings.TrimSpace(in.Name), strings.TrimSpace(in.URL)
	if name == "" || utf8.RuneCountInString(name) > maxDestinationName {
		invalid(w, "name", fmt.Sprintf("Give the destination a name of at most %d characters.", maxDestinationName))
		return
	}
	if !slices.Contains(s.offeredKinds(), in.Kind) {
		invalid(w, "kind", "This build of MailRules does not deliver to that kind of destination.")
		return
	}
	if msg := s.destinationURLProblem(in.Kind, raw); msg != "" {
		invalid(w, "url", msg)
		return
	}
	sec := store.DestinationSecret{URL: raw}
	if in.Kind == store.DestinationWebhook {
		var err error
		if sec.Secret, err = newWebhookSecret(); err != nil {
			internalError(w, r, err)
			return
		}
	}
	d, err := s.store.CreateDestination(r.Context(), s.Master, store.Destination{TenantID: viewer(r).TenantID, Name: name, Kind: in.Kind}, sec, s.now().Unix())
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.publish(r, 0, events.NotificationsChanged, nil)
	var secret *string
	if sec.Secret != "" {
		secret = &sec.Secret
	}
	writeJSON(w, http.StatusCreated, map[string]any{"destination": toDestinationJSON(d, nil), "secret": secret})
}

// handleDestinationDelete removes a destination of the tenant and takes it off its rules.
func (s *server) handleDestinationDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "destination")
	if !ok {
		return
	}
	changed, err := s.store.DeleteDestination(r.Context(), viewer(r).TenantID, id, s.now().Unix())
	if err != nil {
		fail(w, r, err, "destination")
		return
	}
	if len(changed) > 0 {
		s.publish(r, 0, events.RulesChanged, nil)
	}
	s.publish(r, 0, events.NotificationsChanged, nil)
	w.WriteHeader(http.StatusNoContent)
}

// notifyProblem checks the destinations a rule names: each once, each one of the tenant's.
// It answers 400 invalid_input at path itself and returns false when they will not do.
func (s *server) notifyProblem(w http.ResponseWriter, r *http.Request, path string, ids []int64) bool {
	if len(ids) == 0 {
		return true
	}
	if s.notifier() == nil {
		invalid(w, path, "This build of MailRules does not send notifications.")
		return false
	}
	ds, err := s.store.Destinations(r.Context(), viewer(r).TenantID)
	if err != nil {
		internalError(w, r, err)
		return false
	}
	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			invalid(w, path, "Name each destination once.")
			return false
		}
		if !slices.ContainsFunc(ds, func(d store.Destination) bool { return d.ID == id }) {
			invalid(w, path, "No such notification destination.")
			return false
		}
	}
	return true
}
