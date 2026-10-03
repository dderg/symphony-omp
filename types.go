package symphony

import (
	"context"
	"encoding/json"
	"time"
)

type Issue struct {
	ID           string         `json:"id"`
	NativeRef    map[string]any `json:"native_ref"`
	Identifier   string         `json:"identifier"`
	Title        string         `json:"title"`
	Description  *string        `json:"description"`
	Priority     *int           `json:"priority"`
	State        string         `json:"state"`
	BranchName   *string        `json:"branch_name"`
	URL          *string        `json:"url"`
	AssigneeID   *string        `json:"assignee_id"`
	Labels       []string       `json:"labels"`
	BlockedBy    []Blocker      `json:"blocked_by"`
	Dispatchable bool           `json:"dispatchable"`
	CreatedAt    *time.Time     `json:"created_at"`
	UpdatedAt    *time.Time     `json:"updated_at"`
}

type Blocker struct {
	ID         *string `json:"id"`
	Identifier *string `json:"identifier"`
	State      *string `json:"state"`
}

type TrackerConfig struct {
	Kind           string
	Provider       map[string]any
	RequiredLabels []string
	ActiveStates   []string
	TerminalStates []string
}

type Tracker interface {
	FetchIssuesByStates(context.Context, []string) ([]Issue, error)
	FetchIssuesByIDs(context.Context, []string) ([]Issue, error)
	AgentToolSpecs() []ToolSpec
	SecretEnvironmentNames() []string
	ExecuteAgentTool(context.Context, string, json.RawMessage, Issue) ToolResult
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolResult struct {
	Content []ToolContent `json:"content"`
	Details any           `json:"details,omitempty"`
	IsError bool          `json:"-"`
}

type Error struct {
	Category string
	Message  string
}

func (e *Error) Error() string { return e.Category + ": " + e.Message }

func fault(category, message string) error { return &Error{Category: category, Message: message} }
