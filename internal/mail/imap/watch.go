package imap

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/TurtleByte-IN/mailrules/internal/mail"
)

// Watch reports every message in folder with a UID above lastUID, in UID order: first the
// backlog, then arrivals as IDLE (or polling, without IDLE) announces them. It keeps its own
// connection and its own high-water mark, so after a dropped connection it resumes exactly
// where it stopped: nothing is skipped and nothing is sent twice.
//
// It returns when ctx is done, or early for errors that reconnecting cannot fix: wrong
// credentials (mail.ErrAuth), a TLS failure (mail.ErrTLS) or a missing folder (mail.ErrNoFolder).
func (m *Mailbox) Watch(ctx context.Context, folder string, lastUID uint32, out chan<- mail.NewMail) error {
	backoff := m.cfg.BackoffMin
	var validity uint32 // 0 until the first SELECT of this call
	for {
		err := m.watchOnce(ctx, folder, &validity, &lastUID, out, func() { backoff = m.cfg.BackoffMin })
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, mail.ErrAuth) || errors.Is(err, mail.ErrTLS) || errors.Is(err, mail.ErrNoFolder) {
			return err
		}
		wait := backoff + rand.N(backoff/2+1) // #nosec G404 -- jitter, not a secret
		backoff = min(backoff*2, m.cfg.BackoffMax)
		if errors.Is(err, mail.ErrThrottled) {
			wait = m.cfg.ThrottleBackoff
		}
		m.log.Warn("imap watch lost its connection; reconnecting", "account", m.cfg.AccountID, "folder", folder, "retry_in", wait.String(), "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// watchOnce is one connection's lifetime. It only returns with an error.
func (m *Mailbox) watchOnce(ctx context.Context, folder string, validity, lastUID *uint32, out chan<- mail.NewMail, connected func()) error {
	// EXISTS can arrive at any moment, including between a search and the next IDLE; the
	// buffered channel keeps one pending wake-up so it is never lost.
	exists := make(chan struct{}, 1)
	c, err := m.dial(ctx, &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				select {
				case exists <- struct{}{}:
				default:
				}
			}
		},
	})
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	// SELECT, not EXAMINE: some servers do not announce new mail on a read-only selection.
	sel, err := c.Select(folder, nil).Wait()
	if err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) && ie.Type == imap.StatusResponseTypeNo && kindOf(err) == nil {
			return classify("select", errors.Join(mail.ErrNoFolder, err))
		}
		return classify("select", err)
	}
	if *validity != 0 && sel.UIDValidity != *validity {
		// Every UID was reassigned. Start from the newest message rather than re-sort the mailbox.
		*lastUID = uint32(sel.UIDNext) - 1
		m.log.Info("imap folder was rebuilt; resynchronised to its newest message", "account", m.cfg.AccountID,
			"folder", folder, "uidvalidity", sel.UIDValidity, "last_uid", *lastUID)
	}
	*validity = sel.UIDValidity

	catchUp := func() error {
		// "n:*" always matches the newest message, even when its UID is below n; filter it out.
		crit := &imap.SearchCriteria{UID: []imap.UIDSet{{imap.UIDRange{Start: imap.UID(*lastUID + 1), Stop: 0}}}}
		data, err := c.UIDSearch(crit, nil).Wait()
		if err != nil {
			return classify("search", err)
		}
		uids := data.AllUIDs()
		slices.Sort(uids)
		for _, uid := range uids {
			if uint32(uid) <= *lastUID {
				continue
			}
			ref := mail.MsgRef{AccountID: m.cfg.AccountID, Folder: folder, UIDValidity: sel.UIDValidity, UID: uint32(uid)}
			select {
			case out <- mail.NewMail{Ref: ref}:
				*lastUID = uint32(uid)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	if err := catchUp(); err != nil {
		return err
	}
	connected()

	for {
		if m.caps.Idle {
			idle, err := c.Idle()
			if err != nil {
				return classify("idle", err)
			}
			// Leaving IDLE before the server's 30-minute limit, and searching afterwards,
			// also proves the connection is still alive.
			timer := time.NewTimer(m.cfg.IdleRestart)
			select {
			case <-exists:
			case <-timer.C:
			case <-c.Closed():
			case <-ctx.Done():
			}
			timer.Stop()
			if err := idle.Close(); err != nil {
				return classify("idle", err)
			}
			if err := idle.Wait(); err != nil {
				return classify("idle", err)
			}
		} else {
			select {
			case <-time.After(m.cfg.PollInterval):
			case <-c.Closed():
			case <-ctx.Done():
			}
		}
		if err := catchUp(); err != nil {
			return err
		}
	}
}
