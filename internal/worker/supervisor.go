// Package worker runs one supervisor per mail account: it connects, watches the account's
// folder and feeds each arrival to the pipeline, strictly in UID order.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/contacts"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/pipeline"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// QueueSize is how many arrivals may wait between the watcher and the processor.
const QueueSize = 1000

// Account statuses the supervisor sets, as stored in accounts.status.
const (
	StatusLive         = "live"
	StatusReconnecting = "reconnecting"
	StatusAuthFailed   = "auth_failed"
	// StatusCertChanged: the server presents a certificate that is not the one accepted for
	// the account, or, with none accepted, one the system does not trust. It waits until the
	// person accepts the new certificate.
	StatusCertChanged = "cert_changed"
	StatusError       = "error"
	StatusPaused      = "paused" // set by the user; Run is not started for a paused account
	// StatusReconnectNeeded: a mailbox that signs in with OAuth (one-click sign-in) can no
	// longer get in (mail.ErrReconnect): the person revoked access, or the provider refused
	// the token; or the server refused the password of a mailbox a module's sign-in form
	// connected. It waits until the person signs in to the provider again.
	StatusReconnectNeeded = "reconnect_needed"
	// StatusKeyUnavailable: the key service that holds the tenant's key (ext.Module.Keys)
	// cannot be reached, so the mailbox's password cannot be opened (crypto.ErrKeyUnavailable).
	// Unlike auth_failed nothing is wrong with the mailbox: it is retried, with the same
	// growing delay as reconnecting, until the service answers.
	StatusKeyUnavailable = "key_unavailable"
)

// errKeyUnavailable is what a mailbox in StatusKeyUnavailable says, in place of the
// provider's own error, which is logged.
var errKeyUnavailable = errors.New("the key service that protects this mailbox's password cannot be reached; MailRules keeps trying")

// Supervisor owns one account's connection, watcher and processor goroutine.
type Supervisor struct {
	Account store.Account
	Store   *store.Store
	Hub     *events.Hub
	// Open connects to the account's server. It is called again after a failure.
	Open func(ctx context.Context) (mail.Mailbox, error)
	// Pipeline is the template for the account's pipeline; the supervisor fills in
	// Mailbox, Account and Rediscover.
	Pipeline pipeline.Pipeline
	// Moves, when set, looks through the account's filed mail for emails the person moved
	// outside MailRules (moved.Watcher.Sweep), every MovesEvery, on the account's connection
	// and between two emails, like the retry job. nil (the free build) = never.
	Moves      func(ctx context.Context, mb mail.Mailbox, acct store.Account) error
	MovesEvery time.Duration // 0 = 10 minutes

	// Timers. Zero means the default; tests shrink them.
	RetryEvery    time.Duration // the retry job (pipeline.RetryEvery)
	ContactsEvery time.Duration // the Sent-folder rescan (6 h)
	DrainTimeout  time.Duration // how long queued mail may take to finish at shutdown (10 s)
	BackoffMin    time.Duration // first reconnect delay (1 s), doubling up to BackoffMax (5 min)
	BackoffMax    time.Duration

	mu     sync.Mutex
	mb     mail.Mailbox
	status string
	work   sync.Mutex // held while one message (or one retry run) is processed; see Manager.Lock
}

// Mailbox returns the account's open connection, or nil while it is not connected.
func (s *Supervisor) Mailbox() mail.Mailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mb
}

func (s *Supervisor) setMailbox(mb mail.Mailbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mb = mb
}

// account is a copy of the account as the supervisor last saw it. setStatus changes its
// status fields on the supervisor's goroutine while a cleanup Sort runs on another, so a
// read from any other goroutine goes through here (MAI-52).
func (s *Supervisor) account() store.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Account
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run supervises the account until ctx is done or the account needs the user: wrong
// credentials, a one-click sign-in to renew, a certificate to accept, another TLS failure,
// a watch folder that does not exist, or a server the hosted build does not connect to
// (mail.ErrNotPublic); or until the tenant's plan ends (pipeline.ErrHalted), leaving the
// status as it is: the mailbox starts again when a plan is bought. Anything else is
// retried with a growing delay: in key_unavailable while the key service cannot unwrap the
// tenant's key, else in reconnecting. Wrong credentials of a mailbox a module connected
// (store.Account.Module, such as one its sign-in form added with Bridge's password, which
// the person never saw) wait in reconnect_needed, for the person to sign in through the
// module again, rather than in auth_failed.
func (s *Supervisor) Run(ctx context.Context) {
	s.mu.Lock()
	byModule := s.Account.Module || s.Account.OAuth
	// Everything the mailbox's work has the store do is its tenant's (store.WithTenant).
	ctx = store.WithTenant(ctx, s.Account.TenantID)
	s.mu.Unlock()
	backoff := or(s.BackoffMin, time.Second)
	for ctx.Err() == nil {
		mb, err := s.Open(ctx)
		if err == nil {
			backoff = or(s.BackoffMin, time.Second)
			s.setMailbox(mb)
			err = s.session(ctx, mb)
			s.setMailbox(nil)
			_ = mb.Close()
		}
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, pipeline.ErrHalted):
			slog.InfoContext(ctx, "the tenant's plan has ended; the mailbox stopped sorting", "account", s.Account.ID)
			return
		case errors.Is(err, crypto.ErrKeyUnavailable):
			if s.status != StatusKeyUnavailable {
				slog.WarnContext(ctx, "the key service cannot unwrap the mailbox's key; retrying", "account", s.Account.ID, "error", err.Error())
			}
			s.wait(ctx, StatusKeyUnavailable, errKeyUnavailable, &backoff)
			continue
		case errors.Is(err, mail.ErrAuth) && !byModule:
			s.setStatus(ctx, StatusAuthFailed, err)
			return
		case errors.Is(err, mail.ErrReconnect), errors.Is(err, mail.ErrAuth):
			s.setStatus(ctx, StatusReconnectNeeded, err)
			return
		case errors.As(err, new(*mail.CertError)):
			s.setStatus(ctx, StatusCertChanged, err)
			return
		case errors.Is(err, mail.ErrTLS), errors.Is(err, mail.ErrNoFolder):
			s.setStatus(ctx, StatusError, err)
			return
		case errors.Is(err, mail.ErrNotPublic):
			// Only the sentence: it is what the person reads beside the mailbox.
			s.setStatus(ctx, StatusError, mail.ErrNotPublic)
			return
		}
		s.wait(ctx, StatusReconnecting, err, &backoff)
	}
}

// wait sets status for err, then waits backoff before the next try, doubling it up to
// BackoffMax.
func (s *Supervisor) wait(ctx context.Context, status string, err error, backoff *time.Duration) {
	s.setStatus(ctx, status, err)
	select {
	case <-ctx.Done():
	case <-time.After(*backoff):
	}
	*backoff = min(*backoff*2, or(s.BackoffMax, 5*time.Minute))
}

// setStatus stores and announces a status change. err is the reason; nil clears it.
func (s *Supervisor) setStatus(ctx context.Context, status string, err error) {
	if s.status == status {
		return
	}
	s.status = status
	s.mu.Lock()
	s.Account.Status, s.Account.LastError, s.Account.LastEventAt = status, "", time.Now().Unix()
	if err != nil {
		s.Account.LastError = err.Error() // connector errors never contain the password
	}
	acct := s.Account
	s.mu.Unlock()
	slog.InfoContext(ctx, "account status", "account", acct.ID, "status", status, "error", acct.LastError)
	// The status must be written even while the daemon stops.
	if err := s.Store.SetAccountStatus(context.WithoutCancel(ctx), acct.ID, status, acct.LastError, acct.LastEventAt); err != nil {
		slog.ErrorContext(ctx, "could not store the account status", "account", acct.ID, "error", err.Error())
	}
	s.Hub.Publish(acct.TenantID, acct.ID, events.AccountStatus, acct)
}

// discover lists the server's folders and stores them with their special-use roles.
func (s *Supervisor) discover(ctx context.Context, mb mail.Mailbox) error {
	folders, err := mb.Folders(ctx)
	if err != nil {
		return err
	}
	rows := make([]store.Folder, len(folders))
	for i, f := range folders {
		rows[i] = store.Folder{Name: f.Name, Delimiter: f.Delimiter, SpecialUse: f.SpecialUse}
	}
	return s.Store.SaveFolders(ctx, s.Account.ID, rows)
}

// baseline returns the UID to watch from. On first connect, and whenever the folder's
// UIDVALIDITY is not the stored one, that is the newest message there is: only mail that
// arrives from now on is sorted, never a whole mailbox.
func (s *Supervisor) baseline(ctx context.Context, mb mail.Mailbox) (uint32, error) {
	st, err := mb.Status(ctx, s.Account.WatchFolder)
	if err != nil {
		return 0, err
	}
	f, err := s.Store.Folder(ctx, s.Account.ID, s.Account.WatchFolder)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	if err == nil && f.UIDValidity == st.UIDValidity {
		return f.LastUID, nil
	}
	if f.UIDValidity != 0 {
		slog.WarnContext(ctx, "the watched folder was rebuilt on the server; starting again from its newest message",
			"account", s.Account.ID, "folder", s.Account.WatchFolder)
	}
	return st.UIDNext - 1, s.Store.SetFolderPosition(ctx, s.Account.ID, s.Account.WatchFolder, st.UIDValidity, st.UIDNext-1)
}

// syncContacts rescans the Sent folder. The index only sharpens two signals, so a
// failure is logged and the account carries on.
func (s *Supervisor) syncContacts(ctx context.Context, mb mail.Mailbox) {
	folders, err := s.Store.Folders(ctx, s.Account.ID)
	for _, f := range folders {
		if f.SpecialUse == mail.RoleSent {
			_, err = contacts.Sync(ctx, mb, s.Store, s.Account.ID, f.Name)
		}
	}
	if err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "could not rescan sent mail for contacts", "account", s.Account.ID, "error", err.Error())
	}
}

// session is one connection's lifetime: first-connect work, then the watcher feeding the
// processor until ctx is done or the watcher gives up.
func (s *Supervisor) session(ctx context.Context, mb mail.Mailbox) error {
	if err := s.Store.SetAccountCapabilities(ctx, s.Account.ID, mb.Capabilities().All); err != nil {
		return err
	}
	if err := s.discover(ctx, mb); err != nil {
		return err
	}
	lastUID, err := s.baseline(ctx, mb)
	if err != nil {
		return err
	}
	s.syncContacts(ctx, mb)
	s.setStatus(ctx, StatusLive, nil)

	p := s.Pipeline
	p.Mailbox, p.Account = mb, s.Account
	p.Rediscover = func(ctx context.Context) error { return s.discover(ctx, mb) }

	queue := make(chan mail.NewMail, QueueSize)
	watchErr := make(chan error, 1)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go func() {
		watchErr <- mb.Watch(watchCtx, s.Account.WatchFolder, lastUID, queue)
		close(queue)
	}()

	// Mail already queued when ctx ends still gets DrainTimeout to finish.
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(or(s.DrainTimeout, 10*time.Second), cancel) })
	defer stop()
	retry := time.NewTicker(or(s.RetryEvery, pipeline.RetryEvery))
	defer retry.Stop()
	rescan := time.NewTicker(or(s.ContactsEvery, 6*time.Hour))
	defer rescan.Stop()
	// The move sweep, when the build has one. Mailboxes started together do not all sweep
	// at once: each first sweeps at its own offset, then every MovesEvery after a sweep ends.
	var sweep <-chan time.Time
	every := or(s.MovesEvery, 10*time.Minute)
	if s.Moves != nil {
		t := time.NewTimer(time.Duration(uint64(s.Account.ID) * 2654435761 % uint64(every))) //nolint:gosec // G115: ids and durations are positive
		defer t.Stop()
		sweep = t.C
	}
	for {
		select {
		case nm, ok := <-queue:
			if !ok {
				return <-watchErr
			}
			// A message whose outcome could not be recorded must not be passed over: end
			// the session, and the next one watches again from the stored position.
			s.work.Lock()
			err := p.Process(procCtx, nm.Ref)
			s.work.Unlock()
			if err != nil && procCtx.Err() == nil {
				return err
			}
		case <-retry.C:
			s.work.Lock()
			_, err := p.RetryDue(procCtx)
			s.work.Unlock()
			if errors.Is(err, pipeline.ErrHalted) {
				return err
			}
			if err != nil && procCtx.Err() == nil {
				slog.ErrorContext(ctx, "the retry job failed", "account", s.Account.ID, "error", err.Error())
			}
		case <-rescan.C:
			s.syncContacts(procCtx, mb)
		case <-sweep:
			s.work.Lock()
			err := s.Moves(ctx, mb, s.Account)
			s.work.Unlock()
			if err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "could not look for mail moved outside MailRules", "account", s.Account.ID, "error", err.Error())
			}
			sweep = time.After(every)
		}
	}
}
