package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
)

// serve refuses, before it opens anything, a build it cannot serve: the hosted mode with
// no sign-in module, or a module whose routes break the contract.
func TestServeRefusesABadBuild(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	cases := []struct {
		name    string
		env     map[string]string
		modules []ext.Module
		want    string
	}{
		{"cloud mode without a sign-in module", map[string]string{"MAILRULES_MODE": "cloud"}, nil, "MAILRULES_MODE=cloud"},
		{"a webhook that is not public", nil, []ext.Module{{Name: "m", Routes: func(ext.Host) []ext.Route {
			return []ext.Route{{Method: http.MethodPost, Path: "/api/m/hook", Webhook: true, Handler: ok}}
		}}}, "Webhook but not Public"},
		{"a sign-in path that is no route", nil, []ext.Module{{Name: "m", SignIn: "/api/m/start"}}, "SignIn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			err := serve(t.Context(), loadCfg(t, dir, c.env), "test", c.modules)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("serve = %v, want an error saying %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "mailrules.db")); err == nil {
				t.Error("the database was opened before the build was checked")
			}
		})
	}
}
