package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/mail"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// hostResolver answers names from a map, so the tests never look anything up on the network.
type hostResolver map[string][]string

func (r hostResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	answer, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]netip.Addr, len(answer))
	for i, a := range answer {
		out[i] = netip.MustParseAddr(a)
	}
	return out, nil
}

var testHosts = hostResolver{
	"imap.example.test":     {"9.9.9.9"},
	"public.example.test":   {"9.9.9.10", "2620:fe::fe"},
	"intranet.example.test": {"192.168.1.5"},
	"metadata.example.test": {"169.254.169.254"},
}

// publicHostEnv is the daemon in mode ("cloud" or "selfhost") with the resolver above and
// a module that adds mailboxes on any server it names.
func publicHostEnv(t *testing.T, mode string) *env {
	var e *env
	e = newEnvWith(t, func(o *Options) {
		o.Settings.Env.Mode = mode
		o.Resolver = testHosts
		o.ConnectOAuth = func(_ context.Context, acct store.Account, _ *string) (mail.Mailbox, error) {
			e.dialed = acct
			return e.box(acct), nil
		}
	}, serverModule())
	e.signIn()
	return e
}

// serverModule adds a one-click mailbox on the server ?host= names, as a module that keeps
// its mail servers on the operator's own network would.
func serverModule() ext.Module {
	return ext.Module{
		Name: "servers",
		Routes: func(h ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodGet, Path: "/api/servers/connect", Handler: func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				port, _ := strconv.Atoi(q.Get("port"))
				id, err := h.AddMailbox(r.Context(), ext.NewMailbox{Provider: "gmail", Address: q.Get("address"), Host: q.Get("host"), Port: port, Secret: "refresh-1"})
				if err != nil {
					h.WriteJSON(w, http.StatusOK, map[string]string{"failed": err.Error()})
					return
				}
				h.MailboxAdded(w, r, id)
			}}}
		},
		Mailboxes: &ext.MailboxSignIn{Providers: []string{"gmail"}, Connect: "/api/servers/connect",
			Login: func(context.Context, string, string) (ext.MailToken, error) { return ext.MailToken{}, nil }},
	}
}

func genericAccount(host, username string) string {
	return fmt.Sprintf(`{"preset":"generic","host":%q,"username":%q,"password":"app-password-1234"}`, host, username)
}

// In the hosted build, a server the person names that is or resolves to an address off the
// public internet is refused on the connection test, on create and on Edit, before anything
// is dialed and with the same answer whatever listens there. Self-host connects to it.
func TestPrivateHostsInCloudMode(t *testing.T) {
	private := []string{"127.0.0.1", "10.0.0.8", "172.16.4.4", "192.168.0.10", "169.254.169.254", "100.64.1.1", "0.0.0.0",
		"::1", "fd00::5", "fe80::1", "::ffff:127.0.0.1", "intranet.example.test", "metadata.example.test"}

	t.Run("cloud", func(t *testing.T) {
		e := publicHostEnv(t, "cloud")
		for i, host := range private {
			t.Run(host, func(t *testing.T) {
				body := genericAccount(host, fmt.Sprintf("me%d@example.test", i))
				for _, path := range []string{"/api/accounts/test", "/api/accounts"} {
					e.dialed = store.Account{}
					r := e.do(http.MethodPost, path, body)
					if r.status != http.StatusUnprocessableEntity || r.body.Error.Code != "host_not_public" || r.body.Error.Path != "host" ||
						r.body.Error.Message != "The hosted version of MailRules can only connect to mail servers on the public internet." {
						t.Fatalf("POST %s = %d %s", path, r.status, r.raw)
					}
					conform(t, e.doc, "ErrorBody", r.object(t))
					if e.dialed.Host != "" {
						t.Errorf("POST %s dialed %+v before refusing", path, e.dialed)
					}
				}
			})
		}
		if list := e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any); len(list) != 0 {
			t.Fatalf("a refused server was stored: %v", list)
		}

		// A server on the public internet, by address or by name, is connected to.
		e.call(http.MethodPost, "/api/accounts/test", genericAccount("9.9.9.9", "a@example.test"), http.StatusOK)
		e.call(http.MethodPost, "/api/accounts/test", genericAccount("public.example.test", "a@example.test"), http.StatusOK)
		e.call(http.MethodPost, "/api/accounts", genericAccount("imap.example.test", "me@example.test"), http.StatusCreated)
		e.live()

		// Edit refuses to move it onto a private server, and changes nothing.
		for _, host := range []string{"127.0.0.1", "10.1.2.3", "intranet.example.test"} {
			e.dialed = store.Account{}
			e.refuse(http.MethodPatch, "/api/accounts/1", fmt.Sprintf(`{"host":%q}`, host), http.StatusUnprocessableEntity, "host_not_public", "host")
			if e.dialed.Host != "" {
				t.Errorf("Edit to %s dialed %+v before refusing", host, e.dialed)
			}
			if a, _ := e.st.Account(t.Context(), 1); a.Host != "imap.example.test" {
				t.Errorf("a refused move changed the host to %q", a.Host)
			}
		}

		// A name that resolves to a public address when it is checked and a private one
		// when the daemon dials it is refused at dial time, with the same answer.
		e.connectErr = fmt.Errorf("connect to rebind.example.test:993: %w", mail.ErrNotPublic)
		e.refuse(http.MethodPost, "/api/accounts/test", genericAccount("public.example.test", "b@example.test"), http.StatusUnprocessableEntity, "host_not_public", "host")
		e.refuse(http.MethodPost, "/api/accounts/1/test", "", http.StatusUnprocessableEntity, "host_not_public", "host")
		e.connectErr = nil

		// A name that does not resolve is a server that cannot be reached.
		e.connectErr = fmt.Errorf("dial: %w", mail.ErrConnection)
		e.refuse(http.MethodPost, "/api/accounts/test", genericAccount("nowhere.example.test", "c@example.test"), http.StatusUnprocessableEntity, "connection_failed", "host")
		e.connectErr = nil

		// A mailbox a module added is on the module's server, wherever that is.
		r := e.do(http.MethodGet, "/api/servers/connect?address=jo@example.test&host=127.0.0.1&port=1993", "")
		if r.status != http.StatusSeeOther || r.header.Get("Location") != "/#/accounts?added=2" {
			t.Fatalf("module add on 127.0.0.1 = %d %s %s", r.status, r.header.Get("Location"), r.raw)
		}
		if a, _ := e.st.Account(t.Context(), 2); !a.Module || a.Host != "127.0.0.1" {
			t.Errorf("module mailbox = %+v, want Module on 127.0.0.1", a)
		}
		e.call(http.MethodPost, "/api/accounts/2/test", "", http.StatusOK)
		if e.dialed.Host != "127.0.0.1" {
			t.Errorf("the stored test of the module's mailbox dialed %+v", e.dialed)
		}
		if a, _ := e.st.Account(t.Context(), 1); a.Module {
			t.Error("a mailbox the person added is marked as the module's")
		}
	})

	t.Run("selfhost", func(t *testing.T) {
		e := publicHostEnv(t, "selfhost")
		for i, host := range private {
			body := genericAccount(host, fmt.Sprintf("me%d@example.test", i))
			e.dialed = store.Account{}
			e.call(http.MethodPost, "/api/accounts/test", body, http.StatusOK)
			if e.dialed.Host != host {
				t.Errorf("self-host test of %s dialed %+v", host, e.dialed)
			}
		}
		e.call(http.MethodPost, "/api/accounts", genericAccount("127.0.0.1", "me@example.test"), http.StatusCreated)
		e.call(http.MethodPatch, "/api/accounts/1", `{"host":"10.1.2.3"}`, http.StatusOK)
		if a, _ := e.st.Account(t.Context(), 1); a.Host != "10.1.2.3" {
			t.Errorf("self-host move = %q", a.Host)
		}
	})
}
