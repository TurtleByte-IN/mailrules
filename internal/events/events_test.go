package events

import "testing"

func ids(evs []Event) []int64 {
	out := make([]int64, len(evs))
	for i, ev := range evs {
		out[i] = ev.ID
	}
	return out
}

func TestHubReplayAndLive(t *testing.T) {
	var none *Hub
	none.Publish(1, 0, UsageUpdated, nil) // a nil hub drops events

	h := NewHub()
	for i := range 5 {
		h.Publish(1, 0, MessageProcessed, i)
	}
	tests := []struct {
		name   string
		lastID int64
		want   []int64
	}{
		{"new client replays nothing", 0, nil},
		{"reconnect replays what it missed", 3, []int64{4, 5}},
		{"up to date", 5, nil},
		{"id from before a restart replays everything kept", 900, []int64{1, 2, 3, 4, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			missed, _, cancel := h.Subscribe(tt.lastID)
			defer cancel()
			if got := ids(missed); len(got) != len(tt.want) || (len(got) > 0 && (got[0] != tt.want[0] || got[len(got)-1] != tt.want[len(tt.want)-1])) {
				t.Errorf("missed = %v, want %v", got, tt.want)
			}
		})
	}

	missed, live, cancel := h.Subscribe(4)
	h.Publish(2, 7, MessageReview, "six")
	if ev := <-live; len(missed) != 1 || missed[0].ID != 5 || ev.ID != 6 || ev.Name != MessageReview || ev.Data != "six" || ev.TenantID != 2 || ev.AccountID != 7 {
		t.Errorf("missed = %v, live = %+v", ids(missed), ev)
	}
	cancel()
	cancel() // twice is harmless
	if _, open := <-live; open {
		t.Error("channel still open after cancel")
	}
	h.Publish(1, 0, UsageUpdated, nil) // no subscriber left: must not panic
}

func TestHubKeepsLast200AndDropsSlowSubscribers(t *testing.T) {
	h := NewHub()
	_, slow, cancel := h.Subscribe(0)
	defer cancel()
	for range Keep + subBuffer + 50 {
		h.Publish(1, 0, MessageProcessed, nil)
	}
	missed, _, cancel2 := h.Subscribe(1)
	defer cancel2()
	total := int64(Keep + subBuffer + 50)
	if len(missed) != Keep || missed[0].ID != total-Keep+1 || missed[Keep-1].ID != total {
		t.Errorf("kept %d events, %d..%d", len(missed), missed[0].ID, missed[len(missed)-1].ID)
	}
	n := 0
	for range slow { // closed once it fell behind, after delivering what fitted
		n++
	}
	if n != subBuffer {
		t.Errorf("slow subscriber got %d events before being dropped, want %d", n, subBuffer)
	}
}
