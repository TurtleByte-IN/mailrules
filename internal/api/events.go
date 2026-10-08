package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// keepAlive is how often an idle stream gets a comment line, so proxies keep it open.
const keepAlive = 25 * time.Second

// visibleTo reports whether an event may go to v: one of v's tenant (or of none, a nudge
// with no data) and, when it is about one mailbox, one v sees as it is sent, since sharing
// can change while a stream is open.
func (s *server) visibleTo(ctx context.Context, v store.Viewer, ev events.Event) bool {
	switch {
	case ev.TenantID == 0:
		return ev.Data == nil
	case ev.TenantID != v.TenantID:
		return false
	case ev.AccountID == 0:
		return true
	}
	ok, err := s.store.CanSee(ctx, v, ev.AccountID)
	return err == nil && ok
}

// eventData is the JSON of one event as v sees it. Publishers hand the hub Go values; the
// shapes the browser sees are chosen here and are the ones the REST endpoints use.
func (s *server) eventData(ctx context.Context, v store.Viewer, ev events.Event) any {
	switch d := ev.Data.(type) {
	case store.ActivityRow:
		// Read it back: the publisher's row has no actions and no rule name yet.
		if row, err := s.store.ActivityFor(ctx, v, d.Message.ID); err == nil {
			return s.activityJSON(ctx, row)
		}
		return s.activityJSON(ctx, d)
	case store.Action:
		return toActionJSON(d)
	case store.Account:
		// Read it back too: whether it is shared may have changed since it was published.
		if a, err := s.store.VisibleAccount(ctx, v, d.ID); err == nil {
			d.Shared = a.Shared
		}
		return s.accountJSON(ctx, v, d)
	case store.Batch:
		if b, err := s.batchJSON(ctx, v, d); err == nil {
			return b
		}
	case worker.CheckState:
		return s.checkJSON(d)
	}
	return struct{}{}
}

// handleEvents is the Server-Sent Events stream. A client that reconnects sends the id of
// the last event it saw in Last-Event-ID (EventSource does so by itself) and is first sent
// what it missed. A client that falls too far behind is disconnected by the hub and catches
// up the same way. Every event, missed or live, goes only to a subscriber who may see it
// (visibleTo), checked as it is sent.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	last, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	missed, live, cancel := s.Hub.Subscribe(last)
	defer cancel()
	v := viewer(r)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	rc := http.NewResponseController(w)
	send := func(ev events.Event) bool {
		if !s.visibleTo(r.Context(), v, ev) {
			return true
		}
		data, err := json.Marshal(s.eventData(r.Context(), v, ev))
		if err != nil {
			return true // one event that cannot be encoded is not worth the stream
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Name, data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	for _, ev := range missed {
		if !send(ev) {
			return
		}
	}
	ping := time.NewTicker(keepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-live:
			if !ok || !send(ev) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
