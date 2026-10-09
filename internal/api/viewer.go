package api

import (
	"context"
	"net/http"

	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// viewer is the signed-in user as the one a request reads and changes for: their tenant,
// and the mailboxes they see (store.Viewer). Every data handler resolves what it touches
// through it; what the viewer does not see is answered exactly like what does not exist.
func viewer(r *http.Request) store.Viewer { return user(r).Viewer() }

// ctxUser is the signed-in user of a request's context, for code that is handed the
// request's context rather than the request (a module's Host calls). ok is false outside
// a signed-in request.
func ctxUser(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey{}).(store.User)
	return u, ok && u.ID != 0
}

// visibleAccount loads the account the {id} path names when the viewer sees it, answering
// 404 not_found itself, as for a missing one, when they do not.
func (s *server) visibleAccount(w http.ResponseWriter, r *http.Request) (store.Account, bool) {
	id, ok := pathID(w, r, "id", "account")
	if !ok {
		return store.Account{}, false
	}
	a, err := s.store.VisibleAccount(r.Context(), viewer(r), id)
	if err != nil {
		fail(w, r, err, "account")
		return store.Account{}, false
	}
	return a, true
}

// ownAccount loads the account the {id} path names for managing it, which only its owner
// may do: one the viewer does not own (a teammate's shared mailbox) is answered 404
// not_found, as if it did not exist.
func (s *server) ownAccount(w http.ResponseWriter, r *http.Request) (store.Account, bool) {
	a, ok := s.visibleAccount(w, r)
	if ok && a.UserID != user(r).ID {
		notFound(w, "account")
		return store.Account{}, false
	}
	return a, ok
}

// seesAccount reports whether the viewer sees the account; a database failure counts as
// not, after it is logged by the caller's answer.
func (s *server) seesAccount(r *http.Request, accountID int64) bool {
	ok, err := s.store.CanSee(r.Context(), viewer(r), accountID)
	return err == nil && ok
}

// publish sends an event for the viewer's tenant, about one mailbox when accountID is not 0.
func (s *server) publish(r *http.Request, accountID int64, name string, data any) {
	s.Hub.Publish(viewer(r).TenantID, accountID, name, data)
}
