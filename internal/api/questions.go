package api

import (
	"errors"
	"net/http"

	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// questionsOnly refuses a request while no module of the build learns from the person's
// moves (ext.Module.Moves): without one nothing is ever asked.
func (s *server) questionsOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, m := range s.Modules {
			if m.Moves != nil {
				h(w, r)
				return
			}
		}
		writeError(w, http.StatusNotFound, "not_available", "Learning from emails you move in other mail apps is not included in this build of MailRules.", "")
	}
}

type questionJSON struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Mailbox   string `json:"mailbox"`
	From      string `json:"from"`
	Inbox     bool   `json:"inbox"`
	Folder    string `json:"folder"`
	Emails    int    `json:"emails"`
	AskedAt   int64  `json:"asked_at"`
}

func toQuestionJSON(q store.Question) questionJSON {
	return questionJSON{ID: q.ID, AccountID: q.AccountID, Mailbox: q.Mailbox, From: q.From, Inbox: q.Inbox, Folder: q.Folder,
		Emails: q.Emails, AskedAt: q.AskedAt}
}

// handleQuestions lists the open questions about the person's moves, on the mailboxes the
// viewer sees, newest first.
func (s *server) handleQuestions(w http.ResponseWriter, r *http.Request) {
	qs, err := s.store.OpenQuestions(r.Context(), viewer(r))
	if err != nil {
		internalError(w, r, err)
		return
	}
	out := make([]questionJSON, len(qs))
	for i, q := range qs {
		out[i] = toQuestionJSON(q)
	}
	writeJSON(w, http.StatusOK, map[string]any{"questions": out})
}

// handleQuestionAnswer records Yes or No to one open question. Yes stores the sender rule
// "Always do this for <sender>" stores; either way the question is never asked again.
func (s *server) handleQuestionAnswer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id", "question")
	if !ok {
		return
	}
	var in struct {
		Answer string `json:"answer"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Answer != "yes" && in.Answer != "no" {
		invalid(w, "answer", "The answer is yes or no.")
		return
	}
	q, err := s.store.AnswerQuestion(r.Context(), viewer(r), id, in.Answer == "yes", s.now().Unix())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "This question was already answered, or is not one you can see.", "")
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.publish(r, q.AccountID, events.QuestionsChanged, nil)
	writeJSON(w, http.StatusOK, map[string]any{"question": toQuestionJSON(q), "answer": in.Answer})
}
