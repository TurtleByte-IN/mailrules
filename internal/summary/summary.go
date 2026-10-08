// Package summary is the summary email: a daily or weekly email of what MailRules sorted
// per rule, what it trashed and what waits in Needs review, with links into the web app.
// It owns the summary's settings, builds its content from the store, and schedules it: one
// loop checks every minute whether one is due in the user's time zone. Every summary sent
// on schedule is recorded in the summaries table first, so a restart or two checks at once
// never send it twice. It sends through a mailer.Sender and logs counts only, never what
// an email says or who sent it.
package summary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/mailer"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// late is how long after its time a summary may still go out: after a restart, or
	// while its retries run. Later than that it is skipped, and the next one covers its time.
	late = 6 * time.Hour
	// maxAttempts is how many times one summary is tried. The waits between them double
	// from a minute (store.ClaimSummary).
	maxAttempts = 5
	// maxSpan is the longest time one summary covers.
	maxSpan = 31 * 24 * time.Hour
)

// Service sends and shows the summary email.
type Service struct {
	Store         *store.Store
	Sender        mailer.Sender // nil = no mail server is configured
	Missing       []string      // the MAILRULES_SMTP_* settings still to set; empty = it can send
	PublicURL     string        // where links in the summary lead (MAILRULES_PUBLIC_URL)
	DryRunDefault bool          // MAILRULES_DRY_RUN, for while the switch is not stored
	Now           func() time.Time
}

// New returns the service for the daemon's configuration, sending through sender.
func New(st *store.Store, cfg *config.Config, sender mailer.Sender) *Service {
	return &Service{Store: st, Sender: sender, Missing: cfg.SMTPMissing(), PublicURL: cfg.PublicURL, DryRunDefault: cfg.DryRun}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Ready reports whether a summary can be sent.
func (s *Service) Ready() bool { return s.Sender != nil && len(s.Missing) == 0 }

func (s *Service) missing() []string {
	if len(s.Missing) == 0 && s.Sender == nil {
		return []string{"MAILRULES_SMTP_HOST", "MAILRULES_SMTP_FROM"}
	}
	return s.Missing
}

// View is the summary's settings as GET /api/settings shows them.
type View struct {
	Settings
	ToDefault  string   // the admin account's email
	Configured bool     // a mail server is set
	Missing    []string // what to set before one is; never nil
	LastSentAt int64    // 0 = none yet
	NextAt     int64    // 0 = none: off, or no mail server
}

// View returns the settings in force for the user.
func (s *Service) View(ctx context.Context, u store.User) (View, error) {
	cur, err := load(ctx, s.Store)
	if err != nil {
		return View{}, err
	}
	v := View{Settings: cur, ToDefault: u.Email, Configured: s.Ready(), Missing: append([]string{}, s.missing()...)}
	if s.Ready() {
		v.Missing = []string{}
	}
	if v.To == "" {
		v.To = u.Email
	}
	last, err := s.Store.LastSentSummary(ctx, u.ID)
	switch {
	case err == nil:
		v.LastSentAt = last.SentAt
	case !errors.Is(err, store.ErrNotFound):
		return View{}, err
	}
	if cur.Enabled && s.Ready() {
		if _, next, err := cur.slots(s.now()); err == nil {
			v.NextAt = next.Unix()
		}
	}
	return v, nil
}

// Prepare validates a change to the settings and returns what stores it. Nothing is
// stored until commit runs, so the caller can check its other changes first. A problem the
// user can fix is a *settings.Invalid; switching on without a mail server is a *NoSMTP.
func (s *Service) Prepare(ctx context.Context, p Patch) (commit func(context.Context) error, err error) {
	cur, err := load(ctx, s.Store)
	if err != nil {
		return nil, err
	}
	var missing []string
	if !s.Ready() {
		missing = s.missing()
	}
	next, err := apply(cur, p, missing, s.now())
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error { return save(ctx, s.Store, next) }, nil
}

// Preview renders the summary the next scheduled one would be if it went now, and the
// address it would go to.
func (s *Service) Preview(ctx context.Context, u store.User) (Email, Content, string, error) {
	cur, err := load(ctx, s.Store)
	if err != nil {
		return Email{}, Content{}, "", err
	}
	now := s.now()
	start, err := s.start(ctx, u.ID, cur, now)
	if err != nil {
		return Email{}, Content{}, "", err
	}
	email, c, err := s.render(ctx, u.ID, cur, start, now)
	return email, c, recipient(cur, u), err
}

// SendTest sends a summary of the last day now, to the summary's address, whether the
// summary is on or not. It is not recorded as a scheduled summary.
func (s *Service) SendTest(ctx context.Context, u store.User) (Email, string, error) {
	if !s.Ready() {
		return Email{}, "", &NoSMTP{Missing: s.missing()}
	}
	cur, err := load(ctx, s.Store)
	if err != nil {
		return Email{}, "", err
	}
	now := s.now()
	email, c, err := s.render(ctx, u.ID, cur, now.Add(-24*time.Hour), now)
	if err != nil {
		return Email{}, "", err
	}
	to := recipient(cur, u)
	if err := s.Sender.Send(ctx, mailer.Message{To: to, Subject: email.Subject, Text: email.Text, HTML: email.HTML}); err != nil {
		slog.WarnContext(ctx, "test summary email not sent", "error", err.Error())
		return Email{}, "", &SendError{Err: err}
	}
	slog.InfoContext(ctx, "test summary email sent", "sorted", c.Sorted, "trashed", c.TrashedTotal, "review", c.ReviewTotal)
	return email, to, nil
}

// SendError is a summary the mail server did not take: it could not be reached, refused
// the sign-in or the email, or lacks what the settings require. Err says which.
type SendError struct{ Err error }

func (e *SendError) Error() string { return "summary email not sent: " + e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

func recipient(cur Settings, u store.User) string {
	if cur.To != "" {
		return cur.To
	}
	return u.Email
}

// start is where the next scheduled summary begins: where the last one sent ended, while
// the summary has stayed on since; else one period back. Never more than maxSpan back.
func (s *Service) start(ctx context.Context, userID int64, cur Settings, now time.Time) (time.Time, error) {
	start := now.Add(-cur.period())
	last, err := s.Store.LastSentSummary(ctx, userID)
	switch {
	case err == nil && last.SentAt >= cur.EnabledAt && last.PeriodEnd < now.Unix():
		start = time.Unix(last.PeriodEnd, 0)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return start, err
	}
	if earliest := now.Add(-maxSpan); start.Before(earliest) {
		start = earliest
	}
	return start, nil
}

// render builds and renders the summary of [start, end) in the user's zone.
func (s *Service) render(ctx context.Context, userID int64, cur Settings, start, end time.Time) (Email, Content, error) {
	loc, err := zone(cur.TimeZone)
	if err != nil {
		loc = time.UTC
	}
	dryRun, err := s.Store.DryRun(ctx, s.DryRunDefault)
	if err != nil {
		return Email{}, Content{}, err
	}
	c, err := build(ctx, s.Store, userID, start.In(loc), end.In(loc), dryRun)
	if err != nil {
		return Email{}, Content{}, err
	}
	email, err := render(c, s.PublicURL)
	return email, c, err
}

// Run checks the schedule now and then every minute until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "summary email schedule check failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick sends the admin's summary when one is due and has not gone out. It is safe to call
// from two places at once: store.ClaimSummary lets only one of them send.
func (s *Service) Tick(ctx context.Context) error {
	if !s.Ready() {
		return nil
	}
	u, err := s.Store.FirstUser(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil // no admin account yet
	}
	if err != nil {
		return err
	}
	cur, err := load(ctx, s.Store)
	if err != nil || !cur.Enabled {
		return err
	}
	now := s.now()
	due, _, err := cur.slots(now)
	if err != nil {
		return err
	}
	if now.Sub(due) > late || due.Unix() < cur.EnabledAt {
		return nil // too late for this one, or it was due before the summary was switched on
	}
	last, err := s.Store.LastSentSummary(ctx, u.ID)
	switch {
	case err == nil && (last.SentAt >= due.Unix() || now.Sub(time.Unix(last.SentAt, 0)) < cur.period()/2):
		return nil // one went out since this was due, or recently: the schedule was moved after it
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	id, attempt, ok, err := s.Store.ClaimSummary(ctx, u.ID, due.Unix(), now.Unix(), maxAttempts)
	if err != nil || !ok {
		return err
	}
	start, err := s.start(ctx, u.ID, cur, now)
	if err != nil {
		slog.ErrorContext(ctx, "summary email not built", "attempt", attempt, "max_attempts", maxAttempts, "error", err.Error())
		return s.Store.FinishSummary(context.WithoutCancel(ctx), id, false, 0, 0, s.now().Unix())
	}
	return s.send(ctx, cur, u, id, start, now, attempt)
}

// send makes one claimed attempt and records how it ended. One that did not get through is
// recorded as failed, so the next minutes try again, up to maxAttempts; it is logged here.
func (s *Service) send(ctx context.Context, cur Settings, u store.User, id int64, start, end time.Time, attempt int) error {
	failed := func() error {
		return s.Store.FinishSummary(context.WithoutCancel(ctx), id, false, 0, 0, s.now().Unix())
	}
	email, c, err := s.render(ctx, u.ID, cur, start, end)
	if err != nil {
		slog.ErrorContext(ctx, "summary email not built", "attempt", attempt, "max_attempts", maxAttempts, "error", err.Error())
		return failed()
	}
	if err := s.Sender.Send(ctx, mailer.Message{To: recipient(cur, u), Subject: email.Subject, Text: email.Text, HTML: email.HTML}); err != nil {
		slog.WarnContext(ctx, "summary email not sent", "attempt", attempt, "max_attempts", maxAttempts, "error", err.Error())
		return failed()
	}
	// It went out. Should recording that fail, the row stays "sending", which is never
	// tried again: a second copy is worse than a missing record.
	if err := s.Store.FinishSummary(context.WithoutCancel(ctx), id, true, start.Unix(), end.Unix(), s.now().Unix()); err != nil {
		return fmt.Errorf("record the summary sent: %w", err)
	}
	slog.InfoContext(ctx, "summary email sent", "sorted", c.Sorted, "trashed", c.TrashedTotal, "review", c.ReviewTotal, "dry_run", c.DryRun)
	return nil
}
