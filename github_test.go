package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func githubTestPage(next bool, cursor string) map[string]any {
	return map[string]any{"hasNextPage": next, "endCursor": cursor}
}
func githubTestItem(id, project, state string) map[string]any {
	return map[string]any{"__typename": "ProjectV2Item", "id": id, "project": map[string]any{"id": project}, "isArchived": false, "content": map[string]any{"__typename": "Issue", "id": "ISSUE-" + id, "number": 3, "title": "Work", "body": "description", "url": "https://github.com/o/r/issues/3", "state": "OPEN", "locked": false, "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "bad", "repository": map[string]any{"nameWithOwner": "o/r"}, "labels": map[string]any{"nodes": []any{map[string]any{"name": " BUG "}, map[string]any{"name": "bug"}}, "pageInfo": githubTestPage(false, "")}, "assignees": map[string]any{"nodes": []any{map[string]any{"id": "Z"}}, "pageInfo": githubTestPage(false, "")}}, "fieldValues": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2ItemFieldSingleSelectValue", "name": state, "field": map[string]any{"id": "STATUS"}}}, "pageInfo": githubTestPage(false, "")}}
}
func githubTestFields() map[string]any {
	return map[string]any{"node": map[string]any{"__typename": "ProjectV2", "fields": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2SingleSelectField", "id": "STATUS", "name": "Status", "options": []any{map[string]any{"id": "OPT-TODO", "name": "Todo"}, map[string]any{"id": "OPT-DONE", "name": "Done"}}}}, "pageInfo": githubTestPage(false, "")}}}
}
func githubTestConfig(endpoint string) TrackerConfig {
	return TrackerConfig{Kind: "github_projects", Provider: map[string]any{"project_id": "PROJECT", "api_key": "$CUSTOM_TOKEN", "endpoint": endpoint}, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}}
}
func githubTestLookup(name string) (string, bool) {
	if name == "CUSTOM_TOKEN" {
		return "secret-token", true
	}
	return "", false
}
func TestGitHubTLSGraphQLAndTools(t *testing.T) {
	var mu sync.Mutex
	requests := []string{}
	membership := true
	mutations := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret-token" {
			t.Error("missing host auth")
		}
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, req.Query)
		var data any
		switch {
		case strings.Contains(req.Query, "fields(first:"):
			data = githubTestFields()
		case strings.Contains(req.Query, "items(first:"):
			if !strings.Contains(req.Query, "archivedStates:[ARCHIVED,NOT_ARCHIVED]") {
				t.Error("board query must explicitly include archived items")
			}
			item := githubTestItem("ITEM", "PROJECT", "Todo")
			if req.Variables["cursor"] == nil {
				item["fieldValues"].(map[string]any)["pageInfo"] = githubTestPage(true, "FIELDS-2")
				item["content"].(map[string]any)["labels"].(map[string]any)["pageInfo"] = githubTestPage(true, "LABELS-2")
				item["content"].(map[string]any)["assignees"].(map[string]any)["pageInfo"] = githubTestPage(true, "ASSIGNEES-2")
				data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": []any{item}, "pageInfo": githubTestPage(true, "ITEMS-2")}}}
			} else {
				if req.Variables["cursor"] != "ITEMS-2" {
					t.Error("wrong items cursor")
				}
				data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": []any{}, "pageInfo": githubTestPage(false, "")}}}
			}
		case strings.Contains(req.Query, "nodes(ids:"):
			project := "PROJECT"
			if !membership {
				project = "FOREIGN"
			}
			ids := req.Variables["ids"].([]any)
			nodes := []any{}
			for _, id := range ids {
				nodes = append(nodes, githubTestItem(id.(string), project, "Todo"))
			}
			data = map[string]any{"nodes": nodes}
		case strings.Contains(req.Query, "fieldValues(first:100,after:"):
			if req.Variables["cursor"] != "FIELDS-2" {
				t.Error("field cursor")
			}
			data = map[string]any{"node": map[string]any{"id": "ITEM", "fieldValues": map[string]any{"nodes": []any{}, "pageInfo": githubTestPage(false, "")}}}
		case strings.Contains(req.Query, "labels(first:100,after:"):
			if req.Variables["cursor"] != "LABELS-2" {
				t.Error("label cursor")
			}
			data = map[string]any{"node": map[string]any{"id": "ISSUE-ITEM", "labels": map[string]any{"nodes": []any{map[string]any{"name": "Next"}}, "pageInfo": githubTestPage(false, "")}}}
		case strings.Contains(req.Query, "assignees(first:100,after:"):
			if req.Variables["cursor"] != "ASSIGNEES-2" {
				t.Error("assignee cursor")
			}
			data = map[string]any{"node": map[string]any{"id": "ISSUE-ITEM", "assignees": map[string]any{"nodes": []any{map[string]any{"id": "A"}}, "pageInfo": githubTestPage(false, "")}}}
		case strings.HasPrefix(req.Query, "mutation"):
			mutations++
			root := ""
			switch {
			case strings.Contains(req.Query, "updateProjectV2ItemFieldValue"):
				root = "updateProjectV2ItemFieldValue"
				if req.Variables["project"] != "PROJECT" || req.Variables["item"] != "ITEM" || req.Variables["field"] != "STATUS" || req.Variables["option"] != "OPT-DONE" {
					t.Errorf("bad status vars %v", req.Variables)
				}
			case strings.Contains(req.Query, "addComment"):
				root = "addComment"
				if req.Variables["body"] != "Hello" {
					t.Error("bad comment")
				}
			case strings.Contains(req.Query, "closeIssue"):
				root = "closeIssue"
			case strings.Contains(req.Query, "reopenIssue"):
				root = "reopenIssue"
			}
			if root != "updateProjectV2ItemFieldValue" && req.Variables["issue"] != "ISSUE-ITEM" {
				t.Error("trusted native ref instead of provider ID")
			}
			switch root {
			case "updateProjectV2ItemFieldValue":
				data = map[string]any{root: map[string]any{"projectV2Item": map[string]any{"id": "ITEM"}}}
			case "addComment":
				data = map[string]any{root: map[string]any{"commentEdge": map[string]any{"node": map[string]any{"id": "COMMENT", "url": "https://github.com/o/r/issues/3#issuecomment-1"}}}}
			default:
				state := "CLOSED"
				if root == "reopenIssue" {
					state = "OPEN"
				}
				data = map[string]any{root: map[string]any{"issue": map[string]any{"id": "ISSUE-ITEM", "state": state}}}
			}
		default:
			t.Errorf("unexpected query %s", req.Query)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	tracker, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := len(requests)
	if v, e := tracker.FetchIssuesByStates(context.Background(), nil); e != nil || len(v) != 0 {
		t.Fatal(v, e)
	}
	if v, e := tracker.FetchIssuesByIDs(context.Background(), nil); e != nil || len(v) != 0 {
		t.Fatal(v, e)
	}
	if len(requests) != before {
		t.Fatal("empty read sent request")
	}
	issues, err := tracker.FetchIssuesByStates(context.Background(), []string{" todo "})
	if err != nil || len(issues) != 1 {
		t.Fatal(issues, err)
	}
	i := issues[0]
	if i.ID != "ITEM" || i.Identifier != "o/r#3" || !i.Dispatchable || i.AssigneeID == nil || *i.AssigneeID != "A" || fmt.Sprint(i.Labels) != "[bug next]" || i.CreatedAt == nil || i.UpdatedAt != nil || i.Priority != nil || i.BranchName != nil || len(i.BlockedBy) != 0 {
		t.Fatalf("normalization %#v", i)
	}
	i.NativeRef["issue_id"] = "UNTRUSTED"
	for _, call := range []struct{ name, args string }{{"github_get_issue", "{}"}, {"github_set_status", `{"status":" Done "}`}, {"github_add_comment", `{"body":"Hello"}`}, {"github_set_issue_state", `{"state":"CLOSED"}`}, {"github_set_issue_state", `{"state":"OPEN"}`}} {
		if result := tracker.ExecuteAgentTool(context.Background(), call.name, json.RawMessage(call.args), i); result.IsError {
			t.Fatalf("%s: %#v", call.name, result)
		}
	}
	before = len(requests)
	for _, call := range []struct{ name, args string }{{"unknown", "{}"}, {"github_get_issue", `{"id":"FOREIGN"}`}, {"github_set_status", `{"status":"Unknown"}`}, {"github_add_comment", `{"body":""}`}, {"github_set_issue_state", `{"state":"INVALID"}`}} {
		if !tracker.ExecuteAgentTool(context.Background(), call.name, json.RawMessage(call.args), i).IsError {
			t.Fatal("invalid tool accepted")
		}
	}
	if len(requests) != before {
		t.Fatal("invalid arguments reached provider")
	}
	membership = false
	if !tracker.ExecuteAgentTool(context.Background(), "github_add_comment", json.RawMessage(`{"body":"Hello"}`), i).IsError || mutations != 4 {
		t.Fatal("foreign mutation allowed")
	}
	if result := tracker.SecretEnvironmentNames(); !strings.Contains(fmt.Sprint(result), "CUSTOM_TOKEN") {
		t.Fatal(result)
	}
	t.Logf("Observed %d authenticated GraphQL requests and %d scoped mutations over local TLS", len(requests), mutations)
}

func TestGitHubConfigAndErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		modify   func(*TrackerConfig)
		category string
	}{{"missing_secret", func(c *TrackerConfig) { c.Provider["api_key"] = "" }, "missing_tracker_secret"}, {"http_endpoint", func(c *TrackerConfig) { c.Provider["endpoint"] = "http://localhost/graphql" }, "invalid_tracker_config"}, {"missing_project", func(c *TrackerConfig) { c.Provider["project_id"] = "" }, "invalid_tracker_config"}} {
		t.Run(test.name, func(t *testing.T) {
			cfg := githubTestConfig("https://example.invalid")
			test.modify(&cfg)
			_, err := NewGitHubProjects(context.Background(), cfg, nil, githubTestLookup, nil)
			var e *Error
			if !errors.As(err, &e) || e.Category != test.category || strings.Contains(err.Error(), "secret-token") {
				t.Fatal(err)
			}
		})
	}
	for _, test := range []struct {
		name     string
		status   int
		body     string
		category string
	}{{"graphql_partial", 200, `{"data":{"node":null},"errors":[{"message":"secret-token"}]}`, "tracker_response"}, {"graphql_rate", 200, `{"errors":[{"type":"RATE_LIMITED"}]}`, "tracker_rate_limited"}, {"rate", 429, `{}`, "tracker_rate_limited"}, {"http", 500, `secret-token`, "tracker_status"}, {"bad_json", 200, `{`, "tracker_response"}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); fmt.Fprint(w, test.body) }))
			defer server.Close()
			_, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
			var e *Error
			if !errors.As(err, &e) || e.Category != test.category || strings.Contains(err.Error(), "secret-token") {
				t.Fatal(err)
			}
		})
	}
}

func TestGitHubPaginationIntegrity(t *testing.T) {
	yes := true
	cursor := "same"
	seen := map[string]bool{}
	if _, _, e := githubNext(githubInfo{HasNextPage: &yes, EndCursor: &cursor}, seen); e != nil {
		t.Fatal(e)
	}
	if _, _, e := githubNext(githubInfo{HasNextPage: &yes, EndCursor: &cursor}, seen); e == nil {
		t.Fatal("repeated cursor accepted")
	}
	if _, _, e := githubNext(githubInfo{HasNextPage: &yes}, map[string]bool{}); e == nil {
		t.Fatal("missing cursor accepted")
	}
}

func TestGitHubSerializedToolSchemas(t *testing.T) {
	tracker := &githubTracker{}
	raw, err := json.Marshal(tracker.AgentToolSpecs())
	if err != nil {
		t.Fatal(err)
	}
	var specs []struct{ Parameters map[string]json.RawMessage }
	if json.Unmarshal(raw, &specs) != nil {
		t.Fatal("invalid serialized tools")
	}
	if len(specs) != 4 {
		t.Fatal("wrong tool count")
	}
	for _, spec := range specs {
		if string(spec.Parameters["required"]) == "null" {
			t.Fatal("required:null is invalid JSON Schema")
		}
		var required []string
		if json.Unmarshal(spec.Parameters["required"], &required) != nil {
			t.Fatal("required must serialize as array")
		}
	}
}

func TestGitHubAtomicRefreshAndEligibility(t *testing.T) {
	mode := "normal"
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		json.NewDecoder(r.Body).Decode(&req)
		var data any
		if strings.Contains(req.Query, "fields(first:") {
			data = githubTestFields()
		} else if strings.Contains(req.Query, "nodes(ids:") {
			calls++
			ids := req.Variables["ids"].([]any)
			nodes := []any{}
			for _, id := range ids {
				item := githubTestItem(id.(string), "PROJECT", "Todo")
				if mode == "malformed" {
					item["fieldValues"].(map[string]any)["nodes"] = []any{}
				}
				if mode == "closed" {
					item["content"].(map[string]any)["state"] = "CLOSED"
				}
				if mode == "locked" {
					item["content"].(map[string]any)["locked"] = true
				}
				if mode == "archived" {
					item["isArchived"] = true
				}
				if mode == "foreign" {
					item["project"] = map[string]any{"id": "FOREIGN"}
				}
				if mode == "draft" {
					item["content"].(map[string]any)["__typename"] = "DraftIssue"
				}
				nodes = append(nodes, item)
			}
			if mode == "late_error" && calls == 2 {
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}, "errors": []any{map[string]any{"message": "failed"}}})
				return
			}
			data = map[string]any{"nodes": nodes}
		} else {
			item := githubTestItem("ITEM", "PROJECT", "")
			data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": []any{item}, "pageInfo": githubTestPage(false, "")}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	tracker, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"closed", "locked", "archived"} {
		mode = m
		out, e := tracker.FetchIssuesByIDs(context.Background(), []string{"ITEM", "ITEM"})
		if e != nil || len(out) != 1 || out[0].Dispatchable || out[0].State != "Todo" {
			t.Fatalf("%s %v %v", m, out, e)
		}
	}
	for _, m := range []string{"foreign", "draft"} {
		mode = m
		out, e := tracker.FetchIssuesByIDs(context.Background(), []string{"ITEM"})
		if e != nil || len(out) != 0 {
			t.Fatal(m, out, e)
		}
	}
	mode = "malformed"
	if out, e := tracker.FetchIssuesByIDs(context.Background(), []string{"ITEM"}); e == nil || out != nil {
		t.Fatal("malformed refresh did not fail atomically")
	}
	if out, e := tracker.FetchIssuesByStates(context.Background(), []string{"Todo"}); e != nil || len(out) != 0 {
		t.Fatal("malformed state record not omitted", out, e)
	}
	ids := []string{}
	for n := range 101 {
		ids = append(ids, fmt.Sprint(n))
	}
	mode = "late_error"
	calls = 0
	if out, e := tracker.FetchIssuesByIDs(context.Background(), ids); e == nil || out != nil || calls != 2 {
		t.Fatal("late batch failure returned partial result", out, e, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := tracker.FetchIssuesByIDs(ctx, []string{"ITEM"}); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation not propagated", e)
	}
}

func TestGitHubPagedFieldValidationAndArchivedTerminalVisibility(t *testing.T) {
	fieldsPages := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var data any
		if strings.Contains(req.Query, "fields(first:") {
			fieldsPages++
			if req.Variables["cursor"] == nil {
				data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "fields": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2Field", "id": "OTHER", "name": "Other"}}, "pageInfo": githubTestPage(true, "FIELD-METADATA-2")}}}
			} else {
				if req.Variables["cursor"] != "FIELD-METADATA-2" {
					t.Error("incorrect metadata cursor")
				}
				data = githubTestFields()
			}
		} else {
			nodes := []any{}
			if strings.Contains(req.Query, "archivedStates:[ARCHIVED,NOT_ARCHIVED]") {
				item := githubTestItem("ARCHIVED", "PROJECT", "Done")
				item["isArchived"] = true
				nodes = append(nodes, item)
			}
			data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": nodes, "pageInfo": githubTestPage(false, "")}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	tracker, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := tracker.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil || fieldsPages != 2 || len(issues) != 1 || issues[0].Dispatchable || issues[0].State != "Done" {
		t.Fatalf("paged field metadata/archived terminal not preserved: %v %v pages=%d", issues, err, fieldsPages)
	}
}
