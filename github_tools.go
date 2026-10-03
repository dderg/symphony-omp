package symphony

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
)

func (t *githubTracker) AgentToolSpecs() []ToolSpec {
	schema := func(properties map[string]any, required ...string) map[string]any {
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	}
	return []ToolSpec{
		{Name: "github_get_issue", Description: "Read the current issue and its configured project Status.", Parameters: schema(map[string]any{})},
		{Name: "github_set_status", Description: "Set the current project item's Status by option name; does not close the issue.", Parameters: schema(map[string]any{"status": map[string]any{"type": "string", "minLength": 1}}, "status")},
		{Name: "github_add_comment", Description: "Add a comment to the current issue.", Parameters: schema(map[string]any{"body": map[string]any{"type": "string", "minLength": 1}}, "body")},
		{Name: "github_set_issue_state", Description: "Close or reopen the current issue; does not change board Status.", Parameters: schema(map[string]any{"state": map[string]any{"type": "string", "enum": []string{"OPEN", "CLOSED"}}}, "state")},
	}
}
func githubToolResult(value any, failed bool) ToolResult {
	body, _ := json.Marshal(value)
	return ToolResult{Content: []ToolContent{{Type: "text", Text: string(body)}}, Details: value, IsError: failed}
}
func githubToolFailure(err error) ToolResult {
	category, message := "tracker_request", "GitHub operation failed"
	if e, ok := err.(*Error); ok {
		category, message = e.Category, e.Message
	} else if err == context.Canceled {
		category, message = "cancelled", "GitHub operation cancelled; an accepted mutation may have completed"
	} else if err == context.DeadlineExceeded {
		category, message = "timeout", "GitHub operation timed out; an accepted mutation may have completed"
	}
	return githubToolResult(map[string]any{"error": map[string]string{"category": category, "message": message}}, true)
}
func (t *githubTracker) ExecuteAgentTool(ctx context.Context, name string, args json.RawMessage, issue Issue) ToolResult {
	key := ""
	switch name {
	case "github_get_issue":
	case "github_set_status":
		key = "status"
	case "github_add_comment":
		key = "body"
	case "github_set_issue_state":
		key = "state"
	default:
		return githubToolFailure(githubError("unsupported_tool", "unsupported GitHub tool"))
	}
	var arguments map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(args))
	if dec.Decode(&arguments) != nil || arguments == nil {
		return githubToolFailure(githubError("invalid_arguments", "arguments must be an object"))
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return githubToolFailure(githubError("invalid_arguments", "trailing argument data"))
	}
	if (key == "" && len(arguments) != 0) || (key != "" && len(arguments) != 1) {
		return githubToolFailure(githubError("invalid_arguments", "unexpected or missing argument"))
	}
	value := ""
	if key != "" {
		raw, ok := arguments[key]
		if !ok || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
			return githubToolFailure(githubError("invalid_arguments", key+" must be a non-empty string"))
		}
	}
	if name == "github_set_issue_state" && value != "OPEN" && value != "CLOSED" {
		return githubToolFailure(githubError("invalid_arguments", "state must be OPEN or CLOSED"))
	}
	option := ""
	if name == "github_set_status" {
		var ok bool
		option, ok = t.options[githubNorm(value)]
		if !ok {
			return githubToolFailure(githubError("invalid_arguments", "unknown Status option"))
		}
	}
	if strings.TrimSpace(issue.ID) == "" {
		return githubToolFailure(githubError("invalid_arguments", "current issue has no project-item ID"))
	}
	// Refresh derives all target identifiers from GitHub, never from exposed native_ref.
	snapshots, err := t.FetchIssuesByIDs(ctx, []string{issue.ID})
	if err != nil {
		return githubToolFailure(err)
	}
	if len(snapshots) != 1 {
		return githubToolFailure(githubError("out_of_scope", "current item is not an accessible issue in the configured project"))
	}
	fresh := snapshots[0]
	if name == "github_get_issue" {
		return githubToolResult(fresh, false)
	}
	nativeID, ok := fresh.NativeRef["issue_id"].(string)
	if !ok || nativeID == "" {
		return githubToolFailure(githubError("tracker_response", "missing verified issue ID"))
	}
	var query, root string
	vars := map[string]any{}
	switch name {
	case "github_set_status":
		root = "updateProjectV2ItemFieldValue"
		query = `mutation($project:ID!,$item:ID!,$field:ID!,$option:String!){updateProjectV2ItemFieldValue(input:{projectId:$project,itemId:$item,fieldId:$field,value:{singleSelectOptionId:$option}}){projectV2Item{id}}}`
		vars = map[string]any{"project": t.project, "item": fresh.ID, "field": t.fieldID, "option": option}
	case "github_add_comment":
		root = "addComment"
		query = `mutation($issue:ID!,$body:String!){addComment(input:{subjectId:$issue,body:$body}){commentEdge{node{id url}}}}`
		vars = map[string]any{"issue": nativeID, "body": value}
	case "github_set_issue_state":
		root = "closeIssue"
		if value == "OPEN" {
			root = "reopenIssue"
		}
		query = `mutation($issue:ID!){` + root + `(input:{issueId:$issue}){issue{id state}}}`
		vars = map[string]any{"issue": nativeID}
	}
	var data map[string]json.RawMessage
	if err = t.request(ctx, query, vars, &data); err != nil {
		return githubToolFailure(err)
	}
	payload, ok := data[root]
	if !ok || string(payload) == "null" {
		return githubToolFailure(githubError("tracker_response", "missing mutation result"))
	}
	var verified struct {
		ProjectV2Item struct{ ID string }
		CommentEdge   struct{ Node struct{ ID, URL string } }
		Issue         struct{ ID, State string }
	}
	if json.Unmarshal(payload, &verified) != nil ||
		(root == "updateProjectV2ItemFieldValue" && verified.ProjectV2Item.ID != fresh.ID) ||
		(root == "addComment" && (strings.TrimSpace(verified.CommentEdge.Node.ID) == "" || strings.TrimSpace(verified.CommentEdge.Node.URL) == "")) ||
		((root == "closeIssue" || root == "reopenIssue") && (verified.Issue.ID != nativeID || verified.Issue.State != value)) {
		return githubToolFailure(githubError("tracker_response", "invalid mutation result"))
	}
	var output any
	if json.Unmarshal(payload, &output) != nil {
		return githubToolFailure(githubError("tracker_response", "invalid mutation result"))
	}
	return githubToolResult(output, false)
}
