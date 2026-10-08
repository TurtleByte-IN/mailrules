package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/config"
)

// The curl script in docs/api.md must run clean against a fresh daemon. It is run here
// against the real `serve`, so the document cannot drift from what the daemon does.
func TestAPITourInDocsRuns(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	doc, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```bash\n(.*?)\n```").FindSubmatch(doc)
	if m == nil {
		t.Fatal("docs/api.md has no ```bash block")
	}

	// A free local port for this run.
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg, err := config.Load(nil, func(k string) string {
		return map[string]string{"MAILRULES_DATA_DIR": t.TempDir(), "MAILRULES_LISTEN": addr, "LOG_LEVEL": "error"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, "test", nil) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	base := "http://" + addr
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not start")
		}
	}

	cmd := exec.CommandContext(ctx, bash, "-s")
	cmd.Stdin = strings.NewReader(string(m[1]))
	cmd.Env = append(os.Environ(), "BASE="+base, "IMAP_USER=", "IMAP_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "tour complete") {
		t.Fatalf("the tour in docs/api.md failed: %v\n%s", err, out)
	}
}
