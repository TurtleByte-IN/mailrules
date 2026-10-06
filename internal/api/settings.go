package api

import (
	"errors"
	"net/http"

	"github.com/TurtleByte-IN/mailrules/internal/settings"
)

// features are the later-phase features the UI prototype draws. The UI hides each one
// until its flag is true; a flag turns true in the release that builds the feature.
var features = map[string]bool{"digest": false, "notifications": false, "timed_actions": false, "draft_replies": false,
	"billing": false, "unsubscribe": false, "oauth_providers": false}

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
		"retention_days": v.RetentionDays, "openai_base_url": v.OpenAIBaseURL, "ollama_url": v.OllamaURL,
		"keys":     v.Keys, // which provider keys are set; never the keys
		"server":   map[string]string{"version": s.Version, "data_dir": env.DataDir, "listen": env.Listen, "mode": env.Mode},
		"features": features,
	})
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) { s.writeSettings(w, r) }

func (s *server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var p settings.Patch
	if _, ok := readPatch(w, r, map[string]any{
		"dry_run": &p.DryRun, "decider": &p.Decider, "decider_model": &p.DeciderModel, "fallback_model": &p.FallbackModel,
		"composer_model": &p.ComposerModel, "escalate_below": &p.EscalateBelow, "min_confidence": &p.MinConfidence,
		"retention_days": &p.RetentionDays, "openai_base_url": &p.OpenAIBaseURL, "ollama_url": &p.OllamaURL, "keys": &p.Keys,
	}); !ok {
		return
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
