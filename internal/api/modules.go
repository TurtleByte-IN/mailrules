package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/events"
	"github.com/TurtleByte-IN/mailrules/internal/models"
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
			out = append(out, route{method: r.Method, path: r.Path, handler: r.Handler, public: r.Public, webhook: r.Webhook})
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

// Scope refuses an account the signed-in user does not see as it refuses a missing one
// (400 invalid_input at account_id).
func (h host) Scope(w http.ResponseWriter, r *http.Request, in ext.ScopeInput) (ext.Scope, bool) {
	c, ok := h.s.scope(w, r, in)
	return ext.Scope{AccountID: c.AccountID, Folder: c.Folder, Since: c.Since, Limit: c.Limit}, ok
}

// Mail reads an account's mail. The id must come from Scope, which checked that the user
// sees the account.
func (h host) Mail(accountID int64) (ext.Mail, error) {
	mb, err := h.s.reader(accountID)
	if err != nil {
		return nil, err
	}
	return composer.Scanner{Store: h.s.store, Mailbox: mb, AccountID: accountID}, nil
}

// Folders lists an account's folders. The id must come from Scope.
func (h host) Folders(ctx context.Context, accountID int64) ([]string, error) {
	a := &store.Account{ID: accountID}
	return composer.Composer{Store: h.s.store}.Folders(ctx, store.Viewer{}, a)
}

// Rules are the rules of the user's tenant.
func (h host) Rules(ctx context.Context, userID int64) ([]ext.Rule, error) {
	u, err := h.s.store.User(ctx, userID)
	if err != nil {
		return nil, err
	}
	return h.s.store.Rules(ctx, u.TenantID)
}

// errNoUser is what a Host call that needs the signed-in user answers when ctx is not a
// signed-in request's context.
var errNoUser = errors.New("no signed-in user in the context: pass the request's context")

// Composer is the generative model of the signed-in user's tenant; ctx must be the
// request's context.
func (h host) Composer(ctx context.Context) (ext.Generator, error) {
	u, ok := ctxUser(ctx)
	if !ok {
		return nil, errNoUser
	}
	gen, err := h.s.modelSource().Composer(ctx, u.TenantID)
	if err != nil {
		return nil, err // not gen: a nil models.Generator is not a nil ext.Generator
	}
	return gen, nil
}

// RecordUsage books a model call to the signed-in user's tenant, marked when it went out on
// an operator key; ctx must be the request's context.
func (h host) RecordUsage(ctx context.Context, purpose string, u ext.Usage) {
	who, ok := ctxUser(ctx)
	if !ok {
		slog.WarnContext(ctx, "could not record model usage", "error", errNoUser.Error())
		return
	}
	var ledger models.UsageStore = store.Ledger{Store: h.s.store, TenantID: who.TenantID}
	if h.s.Settings != nil {
		ledger = h.s.Settings.Ledger(ctx, who.TenantID)
	}
	if err := ledger.AddUsage(ctx, h.s.now().UTC().Format("2006-01-02"), u.Provider, u.Model, purpose, 1,
		u.TokensIn, u.TokensOut, u.CostUSD); err != nil {
		slog.WarnContext(ctx, "could not record model usage", "error", err.Error())
	}
}

// UsageChanged nudges the open streams of the signed-in user's tenant to read their usage
// again; ctx must be the request's. The nudge carries no data (events.Event).
func (h host) UsageChanged(ctx context.Context) {
	who, ok := ctxUser(ctx)
	if !ok {
		slog.WarnContext(ctx, "could not announce a usage change", "error", errNoUser.Error())
		return
	}
	h.s.Hub.Publish(who.TenantID, 0, events.UsageUpdated, nil)
}

// errIdentity is an Identity a module passed with a field missing.
var errIdentity = errors.New("sign in: identity needs a provider, subject, email and tenant")

// SignIn finds or makes the identity's user and starts the browser's session (ext.Host).
func (h host) SignIn(w http.ResponseWriter, r *http.Request, id ext.Identity) (int64, error) {
	in := store.Identity{Provider: id.Provider, Subject: id.Subject, Email: strings.TrimSpace(id.Email), Tenant: id.Tenant}
	if in.Provider == "" || in.Subject == "" || in.Email == "" || in.Tenant == "" {
		return 0, errIdentity
	}
	u, err := h.s.store.SignInIdentity(r.Context(), in, h.s.now().Unix())
	if err != nil {
		return 0, err
	}
	if err := h.s.startSession(w, r, u.ID); err != nil {
		return 0, fmt.Errorf("sign in: start session: %w", err)
	}
	return u.ID, nil
}

// EndSessions ends every session of the identity's user (ext.Host).
func (h host) EndSessions(ctx context.Context, provider, subject string) error {
	return h.s.store.EndIdentitySessions(ctx, provider, subject)
}

// ForgetIdentity ends the identity's sessions, frees its email and schedules the removal
// of its data (ext.Host).
func (h host) ForgetIdentity(ctx context.Context, provider, subject string) error {
	return h.s.store.ForgetIdentity(ctx, provider, subject, h.s.now().Unix())
}

// SignInFailed sends the browser to the sign-in screen with code (ext.Host).
func (h host) SignInFailed(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/?signin_error="+url.QueryEscape(code), http.StatusSeeOther)
}

// signIn is the module that signs people in, or nil in a build with password sign-in.
func (s *server) signIn() *ext.Module {
	for i := range s.Modules {
		if s.Modules[i].SignIn != "" {
			return &s.Modules[i]
		}
	}
	return nil
}

// CheckModules refuses a build whose modules, or mode (MAILRULES_MODE), the daemon cannot
// serve: a Webhook route that is not Public, a SignIn or SignOut that does not name one of
// its module's Public GET routes, a SignOut without SignIn, more than one module with
// SignIn, and cloud mode with no module that signs people in. The daemon calls it before
// it serves anything.
func CheckModules(mode string, modules []ext.Module) error {
	var signIn []string
	for _, m := range modules {
		publicGet := map[string]bool{}
		if m.Routes != nil {
			for _, r := range m.Routes(host{&server{}}) {
				if r.Webhook && !r.Public {
					return fmt.Errorf("module %s: route %s %s is a Webhook but not Public", m.Name, r.Method, r.Path)
				}
				if r.Public && r.Method == http.MethodGet {
					publicGet[r.Path] = true
				}
			}
		}
		if m.SignOut != "" && m.SignIn == "" {
			return fmt.Errorf("module %s: SignOut is set without SignIn", m.Name)
		}
		for field, path := range map[string]string{"SignIn": m.SignIn, "SignOut": m.SignOut} {
			if path != "" && !publicGet[path] {
				return fmt.Errorf("module %s: %s %q is not one of its Public GET routes", m.Name, field, path)
			}
		}
		if m.SignIn != "" {
			signIn = append(signIn, m.Name)
		}
	}
	if len(signIn) > 1 {
		return fmt.Errorf("more than one module signs people in: %s", strings.Join(signIn, ", "))
	}
	if mode == "cloud" && len(signIn) == 0 {
		return errors.New("MAILRULES_MODE=cloud needs a module that signs people in, and this build has none")
	}
	return nil
}

// moduleStream is eventStream as a module writes it (ext.EventStream).
type moduleStream struct{ es eventStream }

func (m *moduleStream) Send(event string, v any) { m.es.send(event, v) }
func (m *moduleStream) Started() bool            { return m.es.started }
func (m *moduleStream) Fail(err error, code, message string) {
	m.es.send("error", streamFailure(err, code, message))
}
