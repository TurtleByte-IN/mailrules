package api

import (
	"context"
	"log/slog"
	"maps"
	"net/http"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// optionalRoutes are the endpoints of the contract that a module provides (x-module in
// api/openapi.yaml): feature is how the refusal names it while no module serves the route.
var optionalRoutes = []struct{ method, path, module, feature string }{
	{http.MethodPost, "/api/rules/suggest", "suggest", "Suggest from my mail"},
}

// moduleRoutes are the routes the modules serve, then a refusal for each optional route of
// the contract none of them serves.
func (s *server) moduleRoutes() []route {
	var out []route
	served := map[string]bool{}
	for _, m := range s.Modules {
		if m.Routes == nil {
			continue
		}
		for _, r := range m.Routes(host{s}) {
			out = append(out, route{method: r.Method, path: r.Path, handler: r.Handler})
			served[r.Method+" "+r.Path] = true
		}
	}
	for _, o := range optionalRoutes {
		if served[o.method+" "+o.path] {
			continue
		}
		msg := o.feature + " is not included in this build of MailRules."
		out = append(out, route{method: o.method, path: o.path, handler: func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "not_available", msg, "")
		}})
	}
	return out
}

// features are the flags GET /api/settings reports: the later-phase ones, then each
// module's name, on.
func (s *server) features() map[string]bool {
	out := maps.Clone(features)
	for _, m := range s.Modules {
		out[m.Name] = true
	}
	return out
}

// host is what a module's routes get of the daemon (ext.Host): the helpers the built-in
// routes answer with, and read-only access to the mail, the rules and the models.
type host struct{ s *server }

func (h host) ReadJSON(w http.ResponseWriter, r *http.Request, v any) bool { return readJSON(w, r, v) }
func (h host) WriteJSON(w http.ResponseWriter, status int, v any)          { writeJSON(w, status, v) }
func (h host) Invalid(w http.ResponseWriter, path, message string)         { invalid(w, path, message) }
func (h host) Fail(w http.ResponseWriter, r *http.Request, err error)      { h.s.modelFail(w, r, err) }
func (h host) WantsStream(r *http.Request) bool                            { return wantsStream(r) }
func (h host) EventStream(w http.ResponseWriter) ext.EventStream {
	return &moduleStream{eventStream{w: w}}
}
func (h host) UserID(r *http.Request) int64 { return user(r).ID }

func (h host) Scope(w http.ResponseWriter, r *http.Request, in ext.ScopeInput) (ext.Scope, bool) {
	c, ok := h.s.scope(w, r, in)
	return ext.Scope{AccountID: c.AccountID, Folder: c.Folder, Since: c.Since, Limit: c.Limit}, ok
}

func (h host) Mail(accountID int64) (ext.Mail, error) {
	mb, err := h.s.reader(accountID)
	if err != nil {
		return nil, err
	}
	return composer.Scanner{Store: h.s.store, Mailbox: mb, AccountID: accountID}, nil
}

func (h host) Folders(ctx context.Context, accountID int64) ([]string, error) {
	return composer.Composer{Store: h.s.store}.Folders(ctx, &store.Account{ID: accountID})
}

func (h host) Rules(ctx context.Context, userID int64) ([]ext.Rule, error) {
	return h.s.store.Rules(ctx, userID)
}

func (h host) Composer(ctx context.Context) (ext.Generator, error) {
	gen, err := h.s.modelSource().Composer(ctx)
	if err != nil {
		return nil, err // not gen: a nil models.Generator is not a nil ext.Generator
	}
	return gen, nil
}

func (h host) RecordUsage(ctx context.Context, purpose string, u ext.Usage) {
	if err := h.s.store.AddUsage(ctx, h.s.now().UTC().Format("2006-01-02"), u.Provider, u.Model, purpose, 1,
		u.TokensIn, u.TokensOut, u.CostUSD); err != nil {
		slog.WarnContext(ctx, "could not record model usage", "error", err.Error())
	}
}

func (h host) UsageChanged() { h.s.Hub.Publish(events.UsageUpdated, nil) }

// moduleStream is eventStream as a module writes it (ext.EventStream).
type moduleStream struct{ es eventStream }

func (m *moduleStream) Send(event string, v any) { m.es.send(event, v) }
func (m *moduleStream) Started() bool            { return m.es.started }
func (m *moduleStream) Fail(err error, code, message string) {
	m.es.send("error", streamFailure(err, code, message))
}
