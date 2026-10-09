package settings

import "testing"

// In cloud mode an environment key is the operator's. While it is in force for a tenant (the
// tenant stored no key of its own), the settings that say where that key goes are the
// operator's too, so a tenant cannot point openai_base_url at its own server and receive the
// operator's key, nor run the operator's Claude key in a workspace of its choosing. The
// tenant's stored values stay stored, are what its screen shows, and take effect once it
// stores a key of its own. Self-host is unchanged: the environment key is the admin's.
func TestOperatorKeyPinsWhereItIsSent(t *testing.T) {
	const (
		envURL, tenantURL = "https://operator.example/v1", "https://tenant.example/v1"
		envWS, tenantWS   = "wrkspc_operator1", "wrkspc_tenant1"
	)
	tests := []struct {
		name      string
		mode      string
		tenantKey bool // the tenant stores keys of its own
		wantURL   string
		wantWS    string
	}{
		{"cloud, operator key in force", "cloud", false, envURL, envWS},
		{"cloud, the tenant's own key", "cloud", true, tenantURL, tenantWS},
		{"selfhost, environment key", "selfhost", false, tenantURL, tenantWS},
		{"selfhost, stored key", "selfhost", true, tenantURL, tenantWS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			s := newSettings(t, map[string]string{"MAILRULES_MODE": tt.mode,
				"OPENAI_API_KEY": "sk-operator-openai", "OPENAI_BASE_URL": envURL,
				"ANTHROPIC_API_KEY": "sk-ant-operator", "ANTHROPIC_WORKSPACE_ID": envWS})
			url, ws := tenantURL, tenantWS
			p := Patch{OpenAIBaseURL: &url, AnthropicWorkspaceID: &ws}
			if tt.tenantKey {
				p.Keys = map[string]string{"openai_api_key": "sk-tenant-openai", "anthropic_api_key": "sk-ant-tenant"}
			}
			if err := s.Apply(ctx, 1, p); err != nil {
				t.Fatal(err)
			}
			cfg, err := s.Effective(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OpenAIBaseURL != tt.wantURL || cfg.AnthropicWorkspaceID != tt.wantWS {
				t.Errorf("effective base URL %q, workspace %q; want %q, %q", cfg.OpenAIBaseURL, cfg.AnthropicWorkspaceID, tt.wantURL, tt.wantWS)
			}
			// The screen shows what the tenant stored, never the operator's endpoint.
			v, err := s.View(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if v.OpenAIBaseURL != tenantURL || v.AnthropicWorkspaceID != tenantWS {
				t.Errorf("view base URL %q, workspace %q; want the tenant's %q, %q", v.OpenAIBaseURL, v.AnthropicWorkspaceID, tenantURL, tenantWS)
			}
		})
	}
}
