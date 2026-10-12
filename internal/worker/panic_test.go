package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/mail/mailtest"
	"github.com/TurtleByte-IN/mailrules/internal/message"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// errorLines collects the error lines the daemon's logger hands its error writers.
type errorLines struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (l *errorLines) Write(p []byte) (int, error) {
	var m map[string]any
	if err := json.Unmarshal(p, &m); err != nil {
		return 0, err
	}
	l.mu.Lock()
	l.lines = append(l.lines, m)
	l.mu.Unlock()
	return len(p), nil
}

func (l *errorLines) get() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.lines...)
}

// captureErrors makes the default logger the daemon's, handing its errors to the result.
func captureErrors(t *testing.T) *errorLines {
	t.Helper()
	errs, old := &errorLines{}, slog.Default()
	slog.SetDefault(telemetry.NewLogger(io.Discard, "info", errs))
	t.Cleanup(func() { slog.SetDefault(old) })
	return errs
}

// panicky panics once, in Fetch or in Watch, then works.
type panicky struct {
	*mailtest.Mailbox
	in       string // "fetch" or "watch"
	panicked atomic.Bool
}

func (p *panicky) once(in string) {
	if p.in == in && p.panicked.CompareAndSwap(false, true) {
		panic("boom in " + in)
	}
}

func (p *panicky) Fetch(ctx context.Context, ref mail.MsgRef, maxBody int) (*message.Raw, error) {
	p.once("fetch")
	return p.Mailbox.Fetch(ctx, ref, maxBody)
}

func (p *panicky) Watch(ctx context.Context, folder string, lastUID uint32, out chan<- mail.NewMail) error {
	p.once("watch")
	return p.Mailbox.Watch(ctx, folder, lastUID, out)
}

// A panic in a mailbox's work, processing an email or watching the folder, is logged at
// error with its stack, the mailbox and its tenant; the mailbox connects again and sorts
// the email, its processor lock is free, and the daemon goes on.
func TestSupervisorRecoversFromAPanic(t *testing.T) {
	for _, in := range []string{"fetch", "watch"} {
		t.Run(in, func(t *testing.T) {
			errs := captureErrors(t)
			e := newEnv(t)
			p := &panicky{Mailbox: e.mb, in: in}
			e.sup.Open = func(context.Context) (mail.Mailbox, error) { return p, nil }
			m := &Manager{}
			m.Start(t.Context(), e.sup)
			defer m.Stop(e.sup.Account.ID)
			eventually(t, "status live", func() bool {
				a, _ := e.st.Account(t.Context(), e.sup.Account.ID)
				return a.Status == StatusLive
			})
			late := e.mb.Deliver("INBOX", eml("after the panic"))
			eventually(t, "the email sorted after the panic", func() bool { return e.state(t, late) == store.StateActed })
			if !p.panicked.Load() {
				t.Fatal("it never panicked")
			}
			unlock := m.Lock(e.sup.Account.ID) // would wait for good if the panic had kept it
			unlock()

			lines := errs.get()
			if len(lines) != 1 {
				t.Fatalf("error lines = %v, want the panic's", lines)
			}
			l := lines[0]
			stack, _ := l["stack"].(string)
			if l["panic"] != "boom in "+in || !strings.Contains(stack, "(*panicky).once") ||
				l["account"] != float64(e.sup.Account.ID) || l["tenant_id"] != float64(e.sup.Account.TenantID) {
				t.Errorf("logged %v", l)
			}
		})
	}
}

// A cleanup check that panics fails as one the server failed, and is logged with its stack.
func TestCheckRecoversFromAPanic(t *testing.T) {
	errs := captureErrors(t)
	e := newEnv(t)
	m := &Manager{}
	m.Start(t.Context(), e.sup)
	defer m.Stop(e.sup.Account.ID)
	eventually(t, "status live", func() bool {
		a, _ := e.st.Account(t.Context(), e.sup.Account.ID)
		return a.Status == StatusLive
	})
	chk, err := m.StartCheck(Cleanup{AccountID: e.sup.Account.ID, Folder: "INBOX", Limit: 10}, "f",
		func(context.Context, func(composer.CheckProgress)) ([]composer.CheckRow, error) {
			panic("boom in check")
		})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the check to fail", func() bool { return chk.State().Status == CheckFailed })
	lines := errs.get()
	if len(lines) != 1 || lines[0]["panic"] != "boom in check" || lines[0]["stack"] == nil || lines[0]["account"] != float64(e.sup.Account.ID) {
		t.Errorf("error lines = %v", lines)
	}
}
