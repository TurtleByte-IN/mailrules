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

// eventData is the JSON of one event. Publishers hand the hub Go values; the shapes the
// browser sees are chosen here and are the ones the REST endpoints use.
func (s *server) eventData(ctx context.Context, ev events.Event) any {
	switch v := ev.Data.(type) {
	case store.ActivityRow:
		// Read it back: the publisher's row has no actions and no rule name yet.
		if row, err := s.store.ActivityFor(ctx, v.Message.ID); err == nil {
			return s.activityJSON(ctx, row)
		}
		return s.activityJSON(ctx, v)
	case store.Action:
		return toActionJSON(v)
	case store.Account:
		return s.accountJSON(ctx, v)
	case store.Batch:
		if b, err := s.batchJSON(ctx, v); err == nil {
			return b
		}
	case worker.CheckState:
		return s.checkJSON(v)
	}
	return struct{}{}
}

// handleEvents is the Server-Sent Events stream. A client that reconnects sends the id of
// the last event it saw in Last-Event-ID (EventSource does so by itself) and is first sent
// what it missed. A client that falls too far behind is disconnected by the hub and catches
// up the same way.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	last, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	missed, live, cancel := s.Hub.Subscribe(last)
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	rc := http.NewResponseController(w)
	send := func(ev events.Event) bool {
		data, err := json.Marshal(s.eventData(r.Context(), ev))
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
