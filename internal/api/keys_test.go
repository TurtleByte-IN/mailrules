package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// flakyKeys is the test master key behind a key service that can stop answering.
type flakyKeys struct {
	crypto.MasterKey
	down *atomic.Bool
}

var errFlaky = errors.New("rpc error: code = Unavailable")

func (f flakyKeys) Wrap(ctx context.Context, tenantID int64, key []byte) ([]byte, error) {
	if f.down.Load() {
		return nil, errFlaky
	}
	return f.MasterKey.Wrap(ctx, tenantID, key)
}

func (f flakyKeys) Unwrap(ctx context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error) {
	if f.down.Load() {
		return nil, errFlaky
	}
	return f.MasterKey.Unwrap(ctx, tenantID, keyID, wrapped)
}

// A request that needs a secret while the key service cannot be reached answers 503
// key_unavailable, says to try again, and changes nothing.
func TestKeyServiceUnreachable(t *testing.T) {
	var down atomic.Bool
	e := newEnvWith(t, func(o *Options) {
		// Keys kept for no time at all: the first request after the service goes down feels it.
		o.Store = o.Store.WithKeyring(store.NewKeyring(flakyKeys{crypto.MasterKey(storetest.Master), &down}, time.Nanosecond))
	})
	e.connect()
	e.call(http.MethodPost, "/api/accounts/1/test", "", http.StatusOK)

	down.Store(true)
	r := e.do(http.MethodPost, "/api/accounts/1/test", "")
	expect(t, r, http.StatusServiceUnavailable, "key_unavailable")
	if r.header.Get("Retry-After") == "" || strings.Contains(r.body.Error.Message, "rpc error") {
		t.Errorf("Retry-After %q, message %q", r.header.Get("Retry-After"), r.body.Error.Message)
	}
	second := strings.Replace(accountBody, "me@example.test", "other@example.test", 1)
	expect(t, e.do(http.MethodPost, "/api/accounts", second), http.StatusServiceUnavailable, "key_unavailable")
	if n := len(e.call(http.MethodGet, "/api/accounts", "", http.StatusOK)["items"].([]any)); n != 1 {
		t.Errorf("%d mailboxes after a refused one, want 1", n)
	}

	down.Store(false)
	e.call(http.MethodPost, "/api/accounts/1/test", "", http.StatusOK)
}

type keysFor string

func (k keysFor) KeyID() string { return string(k) }
func (keysFor) Wrap(context.Context, int64, []byte) ([]byte, error) {
	return nil, errFlaky
}

func (keysFor) Unwrap(context.Context, int64, string, []byte) ([]byte, error) {
	return nil, errFlaky
}

func TestCheckModulesKeys(t *testing.T) {
	for _, c := range []struct {
		name    string
		modules []ext.Module
		want    string
	}{
		{"none", []ext.Module{{Name: "a"}}, ""},
		{"one", []ext.Module{{Name: "kms", Keys: keysFor("projects/p/cryptoKeys/k")}, {Name: "b"}}, ""},
		{"two", []ext.Module{{Name: "kms", Keys: keysFor("k1")}, {Name: "vault", Keys: keysFor("k2")}}, "more than one module holds the keys: kms, vault"},
		{"the master key's id", []ext.Module{{Name: "kms", Keys: keysFor(crypto.MasterKeyID)}}, "the master key's"},
		{"no id", []ext.Module{{Name: "kms", Keys: keysFor("")}}, "1 to 255"},
		{"an id too long", []ext.Module{{Name: "kms", Keys: keysFor(strings.Repeat("k", 256))}}, "1 to 255"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckModules("selfhost", c.modules)
			if (c.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("CheckModules = %v, want %q", err, c.want)
			}
			p, err := KeyModule(c.modules)
			if c.want == "" && err == nil && (p == nil) != (c.name == "none") {
				t.Errorf("KeyModule = %v", p)
			}
		})
	}
}
