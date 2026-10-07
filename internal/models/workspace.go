package models

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
)

// An Anthropic key is made either for one workspace or for every workspace its owner can
// use. A key of the second kind must name the workspace of each request in the
// anthropic-workspace-id header, or Anthropic refuses it
// (https://platform.claude.com/docs/en/manage-claude/authentication, "Select a workspace",
// read 2026-10-07).

// anthropicErr turns an SDK error into the error the adapters return: a *StatusError for
// an answer from Anthropic, the cause otherwise.
func anthropicErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return newStatusError("anthropic", apiErr.StatusCode, []byte(apiErr.RawJSON()), apiErr.RequestID)
	}
	if err != nil {
		return fmt.Errorf("anthropic: %w", err)
	}
	return nil
}

// IsWorkspaceNeeded reports whether err is Anthropic refusing a request because its key
// covers a whole organisation and the request named no workspace: a 400
// invalid_request_error that says so. The words are Anthropic's ("This API key is not
// scoped to a workspace, so this request must include the anthropic-workspace-id header").
func IsWorkspaceNeeded(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) || se.Provider != "anthropic" || se.Code != http.StatusBadRequest || se.Type != "invalid_request_error" {
		return false
	}
	d := strings.ToLower(se.Detail)
	return strings.Contains(d, workspaceHeader) || strings.Contains(d, "not scoped to a workspace")
}

// Workspace is one of an Anthropic organisation's workspaces.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// WorkspaceAnswer is what Anthropic says about the workspace a key needs: none (the key
// was made for one workspace), or the organisation's workspaces to choose from.
type WorkspaceAnswer struct {
	NoneNeeded bool
	Workspaces []Workspace // the Default Workspace included; never archived ones
}

// AnthropicWorkspaces finds out which workspace a Claude key needs, with List Workspaces
// (https://platform.claude.com/docs/en/api/beta/organization/workspaces/list, read
// 2026-10-07), an Admin API call. The Admin API takes a personal or service-account key
// only when it covers a whole organisation, so a key made for one workspace is refused
// there; so is a key whose role may not list workspaces. A refusal is told apart by asking
// Anthropic to count the tokens of one word without naming a workspace (free of charge):
// that works only for a key that needs none.
type AnthropicWorkspaces struct {
	Deps Deps // its HTTP client, Caller and AnthropicURL
}

// maxWorkspaces is the page size of the one List Workspaces call: the most it allows.
const maxWorkspaces = 1000

// Find answers which workspace apiKey needs. An error means it could not be told: the
// listing was refused and the key cannot run without a workspace either, or Anthropic could
// not be reached. Its text is a *StatusError's, scrubbed, fit for the log.
func (f AnthropicWorkspaces) Find(ctx context.Context, apiKey string) (WorkspaceAnswer, error) {
	ctx = WithPurpose(ctx, "workspace_lookup")
	client := anthropicClient(AnthropicAuth{APIKey: apiKey}, f.Deps)
	call := Call{Provider: "anthropic", Model: "-"}
	var page []anthropic.Workspace
	listErr := f.Deps.Caller.Do(ctx, call, func(ctx context.Context) error {
		res, err := client.Organization.Workspaces.List(ctx, anthropic.OrganizationWorkspaceListParams{Limit: anthropic.Int(maxWorkspaces)},
			anthropicopt.WithQuery("include_default", "true"))
		if res != nil {
			page = res.Data
		}
		return anthropicErr(err)
	})
	var se *StatusError
	switch {
	case listErr == nil:
		out := WorkspaceAnswer{Workspaces: make([]Workspace, 0, len(page))}
		for _, w := range page {
			if w.ArchivedAt.IsZero() && w.ID != "" {
				out.Workspaces = append(out.Workspaces, Workspace{ID: w.ID, Name: w.Name})
			}
		}
		if len(out.Workspaces) == 0 {
			return WorkspaceAnswer{}, errors.New("anthropic: List Workspaces answered no workspace")
		}
		return out, nil
	case !errors.As(listErr, &se):
		return WorkspaceAnswer{}, fmt.Errorf("list workspaces: %w", listErr)
	}

	probeErr := f.Deps.Caller.Do(ctx, call, func(ctx context.Context) error {
		_, err := client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
			Model:    anthropic.Model(DefaultAnthropicModel),
			Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("ping"))},
		})
		return anthropicErr(err)
	})
	if probeErr == nil {
		return WorkspaceAnswer{NoneNeeded: true}, nil
	}
	return WorkspaceAnswer{}, fmt.Errorf("list workspaces: %w (and without a workspace: %w)", listErr, probeErr)
}
