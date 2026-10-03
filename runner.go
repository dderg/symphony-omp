package symphony

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
type AgentEvent struct {
	Event                                            string
	Timestamp                                        time.Time
	PID, SessionID, NativeSessionID, TurnID, Message string
	Usage                                            *TokenUsage
	TurnCount                                        int
}

// RunAttempt binds root, adapter, prompt and agent settings to the launch snapshot.
// Only hook scripts and their timeout are live across workflow reloads.
func RunAttempt(ctx context.Context, w *Workflow, tracker Tracker, workspaces *WorkspaceManager, issue Issue, attempt *int, emit func(AgentEvent)) (err error) {
	manager := *workspaces
	manager.Config = func() Config { live := workspaces.Config(); live.WorkspaceRoot = w.Config.WorkspaceRoot; return live }
	manager.Secrets = tracker.SecretEnvironmentNames()
	path, createErr := manager.Create(ctx, issue.Identifier)
	if path != "" {
		defer func() {
			cleanup, cancel := cleanupContext(ctx)
			defer cancel()
			_ = manager.Hook(cleanup, "after_run", path)
		}()
	}
	if createErr != nil {
		return createErr
	}
	if err = manager.Validate(path); err != nil {
		return err
	}
	if err = manager.Hook(ctx, "before_run", path); err != nil {
		return err
	}
	message, err := RenderPrompt(w, issue, attempt)
	if err != nil {
		return err
	}
	client, err := startRPC(ctx, w.Config, path, tracker, issue, emit)
	if err != nil {
		return err
	}
	defer func() {
		shutdownErr := client.shutdown(err != nil)
		if err == nil {
			err = shutdownErr
		}
	}()
	if err = client.startup(); err != nil {
		client.event("startup_failed", err.Error(), nil)
		return err
	}
	for turn := range w.Config.MaxTurns {
		if err = client.prompt(message); err != nil {
			name := "turn_failed"
			var failure *Error
			if errors.Is(err, context.Canceled) || (errors.As(err, &failure) && failure.Category == "turn_cancelled") {
				name = "turn_cancelled"
			}
			client.event(name, err.Error(), nil)
			return err
		}
		if err = client.stats(); err != nil {
			return err
		}
		client.event("turn_completed", "prompt completed and session settled", nil)
		refreshed, fetchErr := tracker.FetchIssuesByIDs(ctx, []string{issue.ID})
		if fetchErr != nil {
			return fetchErr
		}
		latest, ok := findIssue(refreshed, issue.ID)
		if !ok {
			break
		}
		issue = latest
		effective := workspaces.Config()
		if containsState(effective.Tracker.TerminalStates, issue.State) || !containsState(effective.Tracker.ActiveStates, issue.State) || !routable(effective, issue) || turn+1 >= w.Config.MaxTurns {
			break
		}
		client.issue = issue
		message = fmt.Sprintf("Continue working on %s: %s. The issue remains in %s. Reuse the existing workspace and session; do not restart the original task. Check the current work and finish the remaining requirements.", issue.Identifier, issue.Title, issue.State)
	}
	// Authoritative cumulative totals are refreshed again immediately before EOF.
	return client.stats()
}
