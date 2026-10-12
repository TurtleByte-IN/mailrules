package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

const (
	// noticeEvery is how often the worker looks for notices to hand over: a notice reaches
	// the module within about this long of the rule acting.
	noticeEvery = 2 * time.Second
	// noticeBatch is how many notices one look hands over at most.
	noticeBatch = 50
	// noticeTimeout bounds one Notify call (ext.Notifications.Notify).
	noticeTimeout = 30 * time.Second
	// The pause before a notice the module refused is handed over again doubles from
	// noticeRetryFirst up to noticeRetryMax.
	noticeRetryFirst = 5 * time.Second
	noticeRetryMax   = time.Hour
	// maxReason is how many characters of a failed delivery's reason are kept.
	maxReason = 300
)

// notifications is the build's module that delivers rule notifications, or nil.
func notifications(modules []ext.Module) *ext.Notifications {
	for _, m := range modules {
		if m.Notifications != nil {
			return m.Notifications
		}
	}
	return nil
}

// noticeWorker hands the notices the executor queued to the module that delivers them, at
// least once each: a notice stays waiting until Notify takes it, across restarts.
type noticeWorker struct {
	store      *store.Store
	notify     *ext.Notifications
	deliveries deliveries
	publicURL  string           // MAILRULES_PUBLIC_URL, which Notice.Link starts with
	every      time.Duration    // 0 = noticeEvery
	now        func() time.Time // nil = time.Now
}

func newNoticeWorker(st *store.Store, n *ext.Notifications, hub *events.Hub, publicURL string) *noticeWorker {
	return &noticeWorker{store: st, notify: n, deliveries: deliveries{store: st, hub: hub}, publicURL: publicURL}
}

func (w *noticeWorker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// Run hands over what is waiting now, and then every few seconds, until ctx is done.
func (w *noticeWorker) Run(ctx context.Context) {
	every := w.every
	if every == 0 {
		every = noticeEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := w.once(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "handing notices to the notification module failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// once hands over the notices that are due, oldest first. One that cannot be handed over
// does not hold up the rest; the errors are returned joined.
func (w *noticeWorker) once(ctx context.Context) error {
	waiting, err := w.store.WaitingNotices(ctx, w.clock().Unix(), noticeBatch)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range waiting {
		if ctx.Err() != nil {
			break
		}
		if err := w.hand(ctx, n); err != nil {
			errs = append(errs, fmt.Errorf("notice %d: %w", n.ID, err))
		}
	}
	return errors.Join(errs...)
}

// hand gives one notice to the module. A destination whose address was deleted takes no
// more notices: each is recorded as failed and not handed over.
func (w *noticeWorker) hand(ctx context.Context, n store.Notice) error {
	d, err := w.store.Destination(ctx, n.DestinationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // removed since: its notices went with it
	}
	if err != nil {
		return err
	}
	sec, err := w.store.DestinationSecret(ctx, d.ID)
	if errors.Is(err, store.ErrNoSecret) {
		if err := w.deliveries.Record(ctx, ext.Delivery{NoticeID: n.ID, Reason: "Its address was deleted with the sign-in of the team's only member."}); err != nil {
			return err
		}
		return w.store.NoticeHanded(ctx, n.ID, w.clock().Unix())
	}
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, noticeTimeout)
	err = w.notify.Notify(callCtx, w.notice(n, d, sec), w.deliveries)
	cancel()
	now := w.clock()
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopping: it is handed over again after the restart
		}
		// The module's error names no address or secret (ext.Notifications.Notify).
		slog.WarnContext(ctx, "the notification module did not take a notice; it is handed over again later",
			"notice_id", n.ID, "destination_id", d.ID, "attempts", n.Attempts+1, "error", err.Error())
		return w.store.NoticeRetry(ctx, n.ID, now.Add(retryAfter(n.Attempts)).Unix())
	}
	return w.store.NoticeHanded(context.WithoutCancel(ctx), n.ID, now.Unix())
}

// retryAfter is the pause before a notice the module refused attempts times is handed over
// again.
func retryAfter(attempts int) time.Duration {
	wait := noticeRetryFirst
	for range attempts {
		if wait *= 2; wait >= noticeRetryMax {
			return noticeRetryMax
		}
	}
	return wait
}

// notice is a waiting notice as the module gets it.
func (w *noticeWorker) notice(n store.Notice, d store.Destination, sec store.DestinationSecret) ext.Notice {
	return ext.Notice{
		ID: n.ID, TenantID: n.TenantID,
		Destination: ext.Destination{ID: d.ID, Name: d.Name, Kind: d.Kind, URL: sec.URL, Secret: sec.Secret},
		Mailbox:     n.Mailbox, RuleID: n.RuleID, RuleName: n.RuleName, Actions: n.Actions, Folder: n.Folder,
		From: n.From, FromName: n.FromName, Subject: n.Subject, At: time.Unix(n.CreatedAt, 0).UTC(),
		ActivityID: n.MessageID,
		Link:       strings.TrimRight(w.publicURL, "/") + "/#/activity?id=" + strconv.FormatInt(n.MessageID, 10),
	}
}

// deliveries is where the module records what became of the notices it took
// (ext.Deliveries). Each record tells the tenant's open Settings to list it.
type deliveries struct {
	store *store.Store
	hub   *events.Hub
	now   func() time.Time // nil = time.Now
}

var _ ext.Deliveries = deliveries{}

func (d deliveries) Record(ctx context.Context, r ext.Delivery) error {
	at := r.At
	if at.IsZero() {
		at = time.Now()
		if d.now != nil {
			at = d.now()
		}
	}
	reason := strings.Join(strings.Fields(r.Reason), " ")
	if rs := []rune(reason); len(rs) > maxReason {
		reason = string(rs[:maxReason-1]) + "…"
	}
	if !r.Sent && reason == "" {
		reason = "The notification module gave no reason."
	}
	tenant, err := d.store.RecordDelivery(ctx, r.NoticeID, r.Sent, reason, at.Unix())
	if errors.Is(err, store.ErrNotFound) {
		return ext.ErrNoNotice
	}
	if err != nil {
		return err
	}
	d.hub.Publish(tenant, 0, events.NotificationsChanged, nil)
	return nil
}
