package api

import (
	_ "embed"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/worker"
)

// cleanupRequest reads the body both cleanup endpoints take: which mail to sort.
func (s *server) cleanupRequest(w http.ResponseWriter, r *http.Request) (worker.Cleanup, bool) {
	var in struct {
		AccountID int64  `json:"account_id"`
		Folder    string `json:"folder"`
		Since     *int64 `json:"since"`
		Limit     *int   `json:"limit"`
	}
	if !readJSON(w, r, &in) {
		return worker.Cleanup{}, false
	}
	c := worker.Cleanup{AccountID: in.AccountID, Folder: in.Folder}
	if c.Folder == "" {
		c.Folder = "INBOX"
	}
	switch _, err := s.store.Account(r.Context(), in.AccountID); {
	case err != nil:
		invalid(w, "account_id", "No such account.")
	case in.Since != nil && *in.Since < 0:
		invalid(w, "since", "Give the start as a unix time in seconds, or null for all mail.")
	case in.Limit != nil && *in.Limit < 1:
		invalid(w, "limit", "The limit must be at least 1.")
	default:
		if in.Since != nil && *in.Since > 0 {
			c.Since = time.Unix(*in.Since, 0)
		}
		if in.Limit != nil {
			c.Limit = *in.Limit
		}
		return c, true
	}
	return worker.Cleanup{}, false
}

// handleCleanupPreview counts what a cleanup run would do, per rule, without acting and
// without asking a model: the emails that need one are counted as the calls the run will make.
// ponytail: answers in one request, reading every email of the selection; a folder of
// tens of thousands takes minutes. Give limit, or make the preview a background batch
// with progress like the run if that hurts.
func (s *server) handleCleanupPreview(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cleanupRequest(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	mb, err := s.reader(c.AccountID)
	if err != nil {
		fail(w, r, err, "account")
		return
	}
	rs, err := s.store.Rules(ctx, user(r).ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	senders, err := s.store.SenderRules(ctx, user(r).ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	router, minConfidence := s.modelSource().Live(ctx)
	t := composer.Tester{Store: s.store, Mailbox: mb, AccountID: c.AccountID, BodyChars: s.Settings.Env.BodyChars}
	t.Decider.Router, t.Decider.MinConfidence, t.Decider.Now = router, minConfidence, s.now()
	p, err := t.Preview(ctx, rs, senders, c.Folder, c.Since, c.Limit)
	if err != nil {
		s.modelFail(w, r, err)
		return
	}
	each, err := s.store.DecideCost(ctx)
	if err != nil {
		internalError(w, r, err)
		return
	}
	p.CostUSD = float64(p.ModelCalls) * each
	writeJSON(w, http.StatusOK, p)
}

// handleCleanupRun starts sorting existing mail in the background and answers with the
// run's batch. Progress arrives as batch.progress events.
func (s *server) handleCleanupRun(w http.ResponseWriter, r *http.Request) {
	c, ok := s.cleanupRequest(w, r)
	if !ok {
		return
	}
	b, err := s.Cleanup(r.Context(), c)
	if errors.Is(err, worker.ErrCleanupRunning) {
		writeError(w, http.StatusConflict, "cleanup_running", "A cleanup is already running for this account. Wait for it to finish.", "")
		return
	}
	if err != nil {
		s.modelFail(w, r, err) // an unknown folder, an offline account, the mail server
		return
	}
	bj, err := s.batchJSON(r.Context(), b)
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"batch": bj})
}

var batchKinds = []string{store.BatchLive, store.BatchCleanup, "review", store.BatchCorrection, store.BatchUndo}

// handleBatches lists past batches, newest first: the Cleanup screen's "batches you can undo".
func (s *server) handleBatches(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, limit := q.Get("kind"), 50
	var before int64
	if kind != "" && !slices.Contains(batchKinds, kind) {
		invalid(w, "kind", "Unknown kind of batch.")
		return
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			invalid(w, "cursor", "The cursor must be a positive integer.")
			return
		}
		before = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			invalid(w, "limit", "The limit must be between 1 and 100.")
			return
		}
		limit = n
	}
	rows, err := s.store.Batches(r.Context(), kind, before, limit+1)
	if err != nil {
		internalError(w, r, err)
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := strconv.FormatInt(rows[limit-1].ID, 10)
		next = &c
	}
	items := make([]batchJSON, len(rows))
	for i, b := range rows {
		if items[i], err = s.batchJSON(r.Context(), b); err != nil {
			internalError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

// templates is the rule template gallery, as served: {"items": [...]}. A test saves every
// one of its rules through POST /api/rules/batch.
//
//go:embed templates.json
var templates []byte

func (s *server) handleTemplates(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(templates)
}
