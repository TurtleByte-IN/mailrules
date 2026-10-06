// Package events is the in-process event hub. The pipeline, the action executor and the
// account supervisors publish; the HTTP layer subscribes and writes each event to its
// Server-Sent Events stream. It lives outside internal/api so publishers need not import
// the HTTP layer.
package events

import "sync"

// Event names, as sent in the SSE "event:" field.
const (
	MessageProcessed = "message.processed" // Data: store.ActivityRow
	MessageReview    = "message.review"    // Data: store.ActivityRow
	ActionUndone     = "action.undone"     // Data: store.Action
	AccountStatus    = "account.status"    // Data: store.Account
	BatchProgress    = "batch.progress"
	CheckProgress    = "check.progress" // a cleanup check moved forward or ended; Data: worker.CheckState (rows omitted)
	RulesChanged     = "rules.changed"
	UsageUpdated     = "usage.updated" // Data: nil
)

// Keep is how many past events a reconnecting subscriber can replay.
const Keep = 200

// subBuffer is how far a subscriber may fall behind before it is dropped.
const subBuffer = 256

// Event is one published event. IDs grow by one per event and restart with the daemon.
// Data is the Go value; the HTTP layer chooses its JSON shape.
type Event struct {
	ID   int64
	Name string
	Data any
}

// Hub fans events out to subscribers and remembers the last Keep. A nil *Hub drops
// everything, so publishers need no nil checks.
type Hub struct {
	mu     sync.Mutex
	last   int64
	recent []Event
	subs   map[chan Event]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Publish stores the event and hands it to every subscriber. It never blocks: a
// subscriber that has fallen subBuffer events behind is dropped (its channel closes) and
// catches up by subscribing again with the last id it saw.
func (h *Hub) Publish(name string, data any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last++
	ev := Event{ID: h.last, Name: name, Data: data}
	h.recent = append(h.recent, ev)
	if len(h.recent) > Keep {
		h.recent = h.recent[len(h.recent)-Keep:]
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns the kept events with an id above lastID (the client's Last-Event-ID;
// 0 for a new client replays nothing), then a channel of everything published afterwards.
// A lastID from before a daemon restart is ahead of the hub and replays everything kept.
// cancel ends the subscription and may be called more than once.
func (h *Hub) Subscribe(lastID int64) (missed []Event, live <-chan Event, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lastID > h.last {
		lastID = -1
	}
	if lastID != 0 {
		for _, ev := range h.recent {
			if ev.ID > lastID {
				missed = append(missed, ev)
			}
		}
	}
	ch := make(chan Event, subBuffer)
	h.subs[ch] = struct{}{}
	return missed, ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}
