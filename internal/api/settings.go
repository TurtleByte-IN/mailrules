package api

import (
	"errors"
	"net/http"
	"reflect"

	"github.com/TurtleByte-IN/mailrules/internal/composer"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
)

// features are the later-phase features the UI prototype draws. The UI hides each one
// until its flag is true; a flag turns true in the release that builds the feature, or, for
// a feature a module provides (suggest), while the module is in the build (server.features).
var features = map[string]bool{"digest": false, "notifications": false, "timed_actions": false, "draft_replies": false,
	"billing": false, "unsubscribe": false, "oauth_providers": false, "suggest": false}

// limits are the bounds the daemon enforces on how many emails one run reads, reported so
// the UI's number boxes follow the daemon instead of repeating its numbers (MAI-41).
var limits = map[string]int{"test_default": composer.DefaultLimit, "test_max": composer.MaxLimit, "check_max": composer.MaxLimit}

func (s *server) writeSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.Settings.View(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	env := s.Settings.Env
	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run": v.DryRun, "decider": v.Decider, "decider_model": v.DeciderModel, "fallback_model": v.FallbackModel,
		"composer_model": v.ComposerModel, "escalate_below": v.EscalateBelow, "min_confidence": v.MinConfidence,
		"retention_days": v.RetentionDays, "trash_to_folder": v.TrashToFolder, "leave_own_mail": v.LeaveOwnMail,
		"openai_base_url": v.OpenAIBaseURL, "ollama_url": v.OllamaURL,
		"anthropic_workspace_id": v.AnthropicWorkspaceID, "anthropic_workspace_name": v.AnthropicWorkspaceName,
		"anthropic_workspace_found": v.AnthropicWorkspaceFound,
		"keys":                      v.Keys,     // where each provider key comes from; never the keys
		"warnings":                  v.Warnings, // what the chosen decider still lacks
		"server":                    map[string]string{"version": s.Version, "data_dir": env.DataDir, "listen": env.Listen, "mode": env.Mode},
		"limits":                    limits,
		"features":                  s.features(),
	})
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) { s.writeSettings(w, r) }

// handleAnthropicWorkspaces says which workspace the Claude key in force needs, asking
// Anthropic only while that is not known yet (settings.Settings.LookupWorkspaces).
func (s *server) handleAnthropicWorkspaces(w http.ResponseWriter, r *http.Request) {
	l, err := s.Settings.LookupWorkspaces(r.Context())
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": l.Status, "workspaces": l.Workspaces})
}

func (s *server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var p settings.Patch
	dst := map[string]any{
		"dry_run": &p.DryRun, "decider": &p.Decider, "decider_model": &p.DeciderModel, "fallback_model": &p.FallbackModel,
		"composer_model": &p.ComposerModel, "escalate_below": &p.EscalateBelow, "min_confidence": &p.MinConfidence,
		"retention_days": &p.RetentionDays, "trash_to_folder": &p.TrashToFolder, "leave_own_mail": &p.LeaveOwnMail,
		"openai_base_url": &p.OpenAIBaseURL, "ollama_url": &p.OllamaURL,
		"anthropic_workspace_id": &p.AnthropicWorkspaceID, "keys": &p.Keys,
	}
	sent, ok := readPatch(w, r, dst)
	if !ok {
		return
	}
	// A setting sent as null is forgotten: the environment's default is back in force.
	for name := range sent {
		if name != "keys" && reflect.ValueOf(dst[name]).Elem().IsNil() {
			p.Reset = append(p.Reset, name)
		}
	}
	err := s.Settings.Apply(r.Context(), p)
	var bad *settings.Invalid
	if errors.As(err, &bad) {
		invalid(w, bad.Path, bad.Message)
		return
	}
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.writeSettings(w, r)
}
