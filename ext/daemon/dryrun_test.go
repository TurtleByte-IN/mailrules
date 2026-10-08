package daemon

import (
	"bytes"
	"strings"
	"testing"
)

func TestDryRunCmd(t *testing.T) {
	dir := t.TempDir()
	env := func(def string) func(string) string {
		return func(k string) string {
			return map[string]string{"MAILRULES_DATA_DIR": dir, "MAILRULES_DRY_RUN": def}[k]
		}
	}
	steps := []struct {
		name    string
		args    []string
		def     string // MAILRULES_DRY_RUN
		want    string
		wantErr bool
	}{
		{"unset: on by default", nil, "", "dry-run is on", false},
		{"unset: the environment is the default", nil, "false", "dry-run is off", false},
		{"switch off", []string{"off"}, "", "dry-run is off", false},
		{"the stored switch beats the environment", nil, "true", "dry-run is off", false},
		{"switch on", []string{"on"}, "false", "dry-run is on", false},
		{"bad argument", []string{"maybe"}, "", "", true},
		{"too many arguments", []string{"on", "off"}, "", "", true},
	}
	for _, tt := range steps { // in order: each step builds on the stored switch
		var out bytes.Buffer
		err := dryRunCmd(t.Context(), tt.args, env(tt.def), &out)
		if (err != nil) != tt.wantErr || !strings.HasPrefix(out.String(), tt.want) {
			t.Errorf("%s: output %q, error %v; want %q", tt.name, out.String(), err, tt.want)
		}
	}
}
