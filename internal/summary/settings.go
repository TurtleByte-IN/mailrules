package summary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // the zones a browser reports, also where the system has no zone database (the distroless image)

	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// How often a summary goes out.
const (
	Daily  = "daily"
	Weekly = "weekly"
)

// Weekdays are the values of Settings.Weekday, Sunday first as time.Weekday counts them.
var Weekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// Settings are the summary email's settings, stored as one JSON object in the settings
// table (store.SettingSummary). The zero value is not valid; defaults fills it in.
type Settings struct {
	Enabled   bool   `json:"enabled"`
	Frequency string `json:"frequency"`  // Daily | Weekly
	Weekday   string `json:"weekday"`    // one of Weekdays; used while Weekly
	Time      string `json:"time"`       // HH:MM, 24-hour, in TimeZone
	TimeZone  string `json:"time_zone"`  // IANA name, as the browser reported it
	To        string `json:"to"`         // one address; "" = the admin account's email
	EnabledAt int64  `json:"enabled_at"` // when it was last switched on: a summary due before then is not sent
}

func defaults() Settings {
	return Settings{Frequency: Daily, Weekday: "monday", Time: "08:00", TimeZone: "UTC"}
}

// Optional is a field that may be sent as null: Set says it was sent at all.
type Optional struct {
	Set   bool
	Value string // "" for null
}

// UnmarshalJSON takes a string or null.
func (o *Optional) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = ""
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// Patch is a change to the settings; nil fields, and To unless Set, stay as they are.
type Patch struct {
	Enabled   *bool    `json:"enabled"`
	Frequency *string  `json:"frequency"`
	Weekday   *string  `json:"weekday"`
	Time      *string  `json:"time"`
	TimeZone  *string  `json:"time_zone"`
	To        Optional `json:"to"`
}

// NoSMTP refuses switching the summary on while the daemon has no outgoing mail server.
type NoSMTP struct{ Missing []string }

// ErrNoSMTP is what NoSMTP is, for errors.Is.
var ErrNoSMTP = errors.New("no outgoing mail server is configured")

func (e *NoSMTP) Error() string     { return ErrNoSMTP.Error() + ": " + strings.Join(e.Missing, ", ") }
func (e *NoSMTP) Is(err error) bool { return err == ErrNoSMTP }

// Message is the sentence for a person: what to set, and that a restart reads it.
func (e *NoSMTP) Message() string {
	return "The summary email needs an outgoing mail server. Set " + and(e.Missing) + " in MailRules' environment, then restart it."
}

func and(names []string) string {
	switch len(names) {
	case 0:
		return "the MAILRULES_SMTP_* settings"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

var clock = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// zone loads an IANA zone; "Local" and "" are not names a browser reports.
func zone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("unknown time zone %q", name)
	}
	return time.LoadLocation(name)
}

// load returns the tenant's stored summary settings, the defaults where nothing is stored.
func load(ctx context.Context, st *store.Store, tenantID int64) (Settings, error) {
	v := defaults()
	raw, err := st.Setting(ctx, tenantID, store.SettingSummary)
	if errors.Is(err, store.ErrNotFound) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, fmt.Errorf("decode the summary setting: %w", err)
	}
	return v, nil
}

// apply validates p over cur and returns the settings it makes. A problem the user can fix
// is a *settings.Invalid; switching on without a mail server is a *NoSMTP.
func apply(cur Settings, p Patch, missing []string, now time.Time) (Settings, error) {
	next := cur
	if p.Frequency != nil {
		if *p.Frequency != Daily && *p.Frequency != Weekly {
			return cur, &settings.Invalid{Path: "summary.frequency", Message: "How often must be daily or weekly."}
		}
		next.Frequency = *p.Frequency
	}
	if p.Weekday != nil {
		if !slices.Contains(Weekdays, *p.Weekday) {
			return cur, &settings.Invalid{Path: "summary.weekday", Message: "The day must be a weekday in English, such as monday."}
		}
		next.Weekday = *p.Weekday
	}
	if p.Time != nil {
		if !clock.MatchString(*p.Time) {
			return cur, &settings.Invalid{Path: "summary.time", Message: "Enter the time as HH:MM on a 24-hour clock, such as 08:00."}
		}
		next.Time = *p.Time
	}
	if p.TimeZone != nil {
		if _, err := zone(*p.TimeZone); err != nil {
			return cur, &settings.Invalid{Path: "summary.time_zone", Message: "Unknown time zone. Use an IANA name, such as Europe/Berlin."}
		}
		next.TimeZone = *p.TimeZone
	}
	if p.To.Set {
		to := strings.TrimSpace(p.To.Value)
		if to != "" {
			a, err := mail.ParseAddress(to)
			if err != nil || strings.ContainsAny(to, "\r\n") || !strings.Contains(a.Address, "@") {
				return cur, &settings.Invalid{Path: "summary.to", Message: "Enter one email address, such as you@example.com."}
			}
			to = a.Address
		}
		next.To = to
	}
	if p.Enabled != nil {
		if *p.Enabled && !cur.Enabled {
			if len(missing) > 0 {
				return cur, &NoSMTP{Missing: missing}
			}
			next.EnabledAt = now.Unix()
		}
		next.Enabled = *p.Enabled
	}
	return next, nil
}

// save stores s as the tenant's summary settings.
func save(ctx context.Context, st *store.Store, tenantID int64, s Settings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the summary setting: %w", err)
	}
	return st.SetSetting(ctx, tenantID, store.SettingSummary, string(b))
}

// period is how long one summary covers when there is no last one to start from.
func (s Settings) period() time.Duration {
	if s.Frequency == Weekly {
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// slots returns the latest time a summary was scheduled for at or before now, and the
// first one after now, in the settings' zone. They fall on calendar days there at the
// time set, so a change of clocks moves neither.
func (s Settings) slots(now time.Time) (due, next time.Time, err error) {
	loc, err := zone(s.TimeZone)
	if err != nil {
		return due, next, err
	}
	var hh, mm int
	if _, err := fmt.Sscanf(s.Time, "%d:%d", &hh, &mm); err != nil || !clock.MatchString(s.Time) {
		return due, next, fmt.Errorf("summary time %q: not HH:MM", s.Time)
	}
	n := now.In(loc)
	day, step := n.Day(), 1
	if s.Frequency == Weekly {
		day -= (int(n.Weekday()) - slices.Index(Weekdays, s.Weekday) + 7) % 7
		step = 7
	}
	at := func(day int) time.Time { return time.Date(n.Year(), n.Month(), day, hh, mm, 0, 0, loc) }
	if at(day).After(now) {
		day -= step
	}
	return at(day), at(day + step), nil
}
