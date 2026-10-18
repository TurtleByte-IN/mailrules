package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// halt makes the tenant's plan ended while *ended is true, or fail the check with *err; it
// records the tenant and time it was asked about.
func (e *env) halt(ended *bool, err *error, asked *[]int64) {
	e.p.Halted = func(_ context.Context, tenantID int64, now time.Time) (bool, error) {
		if !now.Equal(e.now) {
			e.t.Errorf("Halted asked at %v, want the pipeline's now %v", now, e.now)
		}
		*asked = append(*asked, tenantID)
		return *ended, *err
	}
}

func (e *env) messages() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(e.t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) lastUID() uint32 {
	e.t.Helper()
	f, err := e.st.Folder(e.t.Context(), e.p.Account.ID, "INBOX")
	if errors.Is(err, store.ErrNotFound) {
		return 0
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return f.LastUID
}

// While the tenant's plan has ended, new mail is left as it is: nothing is recorded, no
// action applied, and the folder's position stays, so the email is sorted when the plan
// runs again. A check that fails sorts nothing either; a plan that runs sorts as before.
func TestHaltedPlanSortsNothing(t *testing.T) {
	broken := errors.New("database is locked")
	for _, c := range []struct {
		name      string
		ended     bool
		err       error
		wantErr   error
		wantMoved bool
	}{
		{name: "a plan that runs sorts", wantMoved: true},
		{name: "an ended plan sorts nothing", ended: true, wantErr: ErrHalted},
		{name: "a failed check sorts nothing", err: broken, wantErr: broken},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			e.primary.DecideFunc = answer(e.food.ID, 0.93)
			var asked []int64
			ended, checkErr := c.ended, c.err
			e.halt(&ended, &checkErr, &asked)
			ref := e.deliver("orders@swiggy.example", "order 1")
			err := e.p.Process(ctx, ref)
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Fatalf("Process = %v, want %v", err, c.wantErr)
			}
			if len(asked) != 1 || asked[0] != e.p.Account.TenantID {
				t.Errorf("Halted asked about tenants %v, want [%d]", asked, e.p.Account.TenantID)
			}
			if moved := len(e.exec.applied()) > 0; moved != c.wantMoved {
				t.Errorf("applied %v, want moved %v", e.exec.applied(), c.wantMoved)
			}
			if !c.wantMoved {
				if n := e.messages(); n != 0 {
					t.Errorf("%d messages recorded while halted", n)
				}
				if uid := e.lastUID(); uid != 0 {
					t.Errorf("last_uid = %d while halted, want the position unmoved", uid)
				}
				if n := len(e.primary.Requests()); n != 0 {
					t.Errorf("a model was asked %d times while halted", n)
				}
				// The plan runs again: the same email, sent again from the stored position,
				// is sorted.
				ended, checkErr = false, nil
				if err := e.p.Process(ctx, ref); err != nil {
					t.Fatal(err)
				}
			}
			if row := e.row(ref); row.Message.State != store.StateActed || e.lastUID() != ref.UID {
				t.Errorf("after the plan runs: state %s, last_uid %d; want acted at %d", row.Message.State, e.lastUID(), ref.UID)
			}
		})
	}
}

// The retry job, a cleanup's Sort and its SortSaved sort nothing while the plan has ended.
func TestHaltedPlanRetriesAndCleansNothing(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	// A message that failed once waits for the retry job.
	e.primary.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, &models.StatusError{Provider: "fake", Code: 503}
	}
	failed := e.deliver("orders@swiggy.example", "order 1")
	if err := e.p.Process(ctx, failed); err != nil {
		t.Fatal(err)
	}
	e.primary.DecideFunc = answer(e.food.ID, 0.93)
	existing := e.deliver("orders@swiggy.example", "order 2")
	out := e.settle(existing)
	e.now = e.now.Add(RetryEvery)
	var asked []int64
	ended, checkErr := true, error(nil)
	e.halt(&ended, &checkErr, &asked)
	callsBefore := len(e.primary.Requests())
	for name, run := range map[string]func() error{
		"RetryDue":  func() error { _, err := e.p.RetryDue(ctx); return err },
		"Sort":      func() error { return e.p.Sort(ctx, existing) },
		"SortSaved": func() error { _, err := e.sortSaved(ctx, existing, out); return err },
	} {
		if err := run(); !errors.Is(err, ErrHalted) {
			t.Errorf("%s = %v, want ErrHalted", name, err)
		}
	}
	if row := e.row(failed); row.Message.State != store.StateError || row.Message.Attempts != 0 {
		t.Errorf("the failed message = %s after %d tries, want it waiting as it was", row.Message.State, row.Message.Attempts)
	}
	if n := e.messages(); n != 1 {
		t.Errorf("%d messages recorded, want only the one that failed before", n)
	}
	if n := len(e.primary.Requests()) - callsBefore; n != 0 || len(e.exec.applied()) != 0 {
		t.Errorf("while halted: %d model calls, applied %v", n, e.exec.applied())
	}
}
