package symphony

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const githubPage = `pageInfo { hasNextPage endCursor }`
const githubItemFields = `__typename id isArchived project { id } content { __typename ... on Issue { id number title body url state locked createdAt updatedAt repository { nameWithOwner } labels(first:100) { nodes { name } pageInfo { hasNextPage endCursor } } assignees(first:100) { nodes { id } pageInfo { hasNextPage endCursor } } } } fieldValues(first:100) { nodes { __typename ... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2FieldCommon { id } } } } pageInfo { hasNextPage endCursor } }`

type githubTracker struct {
	project, endpoint, token, statusName, fieldID string
	client                                        *http.Client
	logger                                        *slog.Logger
	secrets                                       []string
	options                                       map[string]string
}
type githubInfo struct {
	HasNextPage *bool   `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}
type githubConnection[T any] struct {
	Nodes    []T        `json:"nodes"`
	PageInfo githubInfo `json:"pageInfo"`
}
type githubField struct {
	Type     string `json:"__typename"`
	ID, Name string
	Options  []struct{ ID, Name string }
}
type githubValue struct {
	Type  string `json:"__typename"`
	Name  *string
	Field struct{ ID string }
}
type githubIssue struct {
	Type                 string `json:"__typename"`
	ID                   string
	Number               int
	Title                string
	Body, URL            *string
	State                string
	Locked               *bool
	CreatedAt, UpdatedAt *string
	Repository           struct{ NameWithOwner string }
	Labels               githubConnection[struct{ Name string }]
	Assignees            githubConnection[struct{ ID string }]
}
type githubItem struct {
	Type        string `json:"__typename"`
	ID          string
	IsArchived  *bool
	Project     struct{ ID string }
	Content     *githubIssue
	FieldValues githubConnection[githubValue]
}

func githubError(category, message string) error { return &Error{Category: category, Message: message} }
func githubNorm(s string) string                 { return strings.ToLower(strings.TrimSpace(s)) }
func NewGitHubProjects(ctx context.Context, cfg TrackerConfig, client *http.Client, lookupEnv func(string) (string, bool), logger *slog.Logger) (Tracker, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	getenv := func(name string) string { v, _ := lookupEnv(name); return v }
	if cfg.Kind != "github_projects" {
		return nil, githubError("unsupported_tracker_kind", "expected github_projects")
	}
	t := &githubTracker{endpoint: "https://api.github.com/graphql", statusName: "Status", client: client, logger: logger, options: map[string]string{}, secrets: []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}}
	get := func(key string, fallback string, expand bool) (string, error) {
		v, ok := cfg.Provider[key]
		if !ok {
			return fallback, nil
		}
		s, ok := v.(string)
		if !ok {
			return "", githubError("invalid_tracker_config", key+" must be a string")
		}
		if expand && strings.HasPrefix(s, "$") {
			s = getenv(strings.TrimPrefix(s, "$"))
		}
		return s, nil
	}
	var err error
	if t.project, err = get("project_id", "", true); err != nil {
		return nil, err
	}
	if strings.TrimSpace(t.project) == "" {
		return nil, githubError("invalid_tracker_config", "project_id is required")
	}
	if t.endpoint, err = get("endpoint", t.endpoint, false); err != nil {
		return nil, err
	}
	u, e := url.Parse(t.endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, githubError("invalid_tracker_config", "endpoint must be an HTTPS URL without credentials or fragment")
	}
	if t.statusName, err = get("status_field", t.statusName, false); err != nil {
		return nil, err
	}
	if strings.TrimSpace(t.statusName) == "" {
		return nil, githubError("invalid_tracker_config", "status_field must be non-empty")
	}
	if v, ok := cfg.Provider["api_key"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, githubError("invalid_tracker_config", "api_key must be a string")
		}
		if strings.HasPrefix(s, "$") {
			name := strings.TrimPrefix(s, "$")
			t.secrets = append(t.secrets, name)
			s = getenv(name)
		}
		t.token = s
	} else {
		t.token = getenv("GH_TOKEN")
		if strings.TrimSpace(t.token) == "" {
			t.token = getenv("GITHUB_TOKEN")
		}
	}
	if strings.TrimSpace(t.token) == "" {
		return nil, githubError("missing_tracker_secret", "GitHub authentication is required")
	}
	if err = t.loadFields(ctx, append(append([]string{}, cfg.ActiveStates...), cfg.TerminalStates...)); err != nil {
		return nil, err
	}
	return t, nil
}
func (t *githubTracker) request(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return githubError("tracker_request", "cannot construct GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/json")
	// Never forward the host credential to a redirect destination.
	client := *t.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return githubError("tracker_request", "GitHub transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 429 || (res.StatusCode == 403 && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "")) {
		return githubError("tracker_rate_limited", "GitHub rate limit exceeded")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return githubError("tracker_status", fmt.Sprintf("GitHub HTTP status %d", res.StatusCode))
	}
	var envelope struct {
		Data   json.RawMessage
		Errors []struct {
			Type       string
			Message    string
			Extensions struct{ Code string }
		}
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, 32<<20))
	if dec.Decode(&envelope) != nil {
		return githubError("tracker_response", "invalid GraphQL response")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return githubError("tracker_response", "invalid trailing GraphQL response")
	}
	if len(envelope.Errors) > 0 {
		for _, e := range envelope.Errors {
			if strings.Contains(strings.ToUpper(e.Type+" "+e.Extensions.Code), "RATE_LIMIT") {
				return githubError("tracker_rate_limited", "GitHub rate limit exceeded")
			}
		}
		return githubError("tracker_response", "GitHub returned GraphQL errors")
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, out) != nil {
		return githubError("tracker_response", "missing or invalid GraphQL data")
	}
	return nil
}
func githubNext(info githubInfo, seen map[string]bool) (string, bool, error) {
	if info.HasNextPage == nil {
		return "", false, githubError("tracker_pagination", "missing pageInfo")
	}
	if !*info.HasNextPage {
		return "", false, nil
	}
	if info.EndCursor == nil || strings.TrimSpace(*info.EndCursor) == "" || seen[*info.EndCursor] {
		return "", false, githubError("tracker_pagination", "missing or repeated cursor")
	}
	seen[*info.EndCursor] = true
	return *info.EndCursor, true, nil
}
func (t *githubTracker) loadFields(ctx context.Context, states []string) error {
	cursor := ""
	seen := map[string]bool{}
	matches := 0
	for {
		var data struct {
			Node *struct {
				Type   string `json:"__typename"`
				Fields githubConnection[githubField]
			}
		}
		err := t.request(ctx, `query($project:ID!,$cursor:String){node(id:$project){__typename ... on ProjectV2 { fields(first:100,after:$cursor){nodes{__typename ... on ProjectV2FieldCommon{id name} ... on ProjectV2SingleSelectField{options{id name}}} `+githubPage+`}}}}`, map[string]any{"project": t.project, "cursor": nullableCursor(cursor)}, &data)
		if err != nil {
			return err
		}
		if data.Node == nil || data.Node.Type != "ProjectV2" {
			return githubError("invalid_tracker_config", "project_id must identify an accessible ProjectV2")
		}
		for _, f := range data.Node.Fields.Nodes {
			if f.Name == t.statusName {
				matches++
				if f.Type != "ProjectV2SingleSelectField" || f.ID == "" {
					return githubError("invalid_tracker_config", "Status field must be single-select")
				}
				t.fieldID = f.ID
				for _, o := range f.Options {
					key := githubNorm(o.Name)
					if key == "" || o.ID == "" {
						return githubError("invalid_tracker_config", "invalid Status option")
					}
					if _, ok := t.options[key]; ok {
						return githubError("invalid_tracker_config", "ambiguous Status options")
					}
					t.options[key] = o.ID
				}
			}
		}
		next, more, err := githubNext(data.Node.Fields.PageInfo, seen)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		cursor = next
	}
	if matches != 1 {
		return githubError("invalid_tracker_config", "Status field missing or ambiguous")
	}
	for _, s := range states {
		if _, ok := t.options[githubNorm(s)]; !ok {
			return githubError("invalid_tracker_config", "configured state is not a Status option")
		}
	}
	return nil
}
func nullableCursor(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (t *githubTracker) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	out := []Issue{}
	if len(states) == 0 {
		return out, nil
	}
	wanted := map[string]bool{}
	for _, s := range states {
		wanted[githubNorm(s)] = true
	}
	seenIDs := map[string]bool{}
	cursor := ""
	seen := map[string]bool{}
	for {
		var data struct {
			Node *struct {
				Type  string `json:"__typename"`
				Items githubConnection[githubItem]
			}
		}
		err := t.request(ctx, `query($project:ID!,$cursor:String){node(id:$project){__typename ... on ProjectV2{items(first:100,after:$cursor,archivedStates:[ARCHIVED,NOT_ARCHIVED]){nodes{`+githubItemFields+`} `+githubPage+`}}}}`, map[string]any{"project": t.project, "cursor": nullableCursor(cursor)}, &data)
		if err != nil {
			return nil, err
		}
		if data.Node == nil || data.Node.Type != "ProjectV2" {
			return nil, githubError("invalid_tracker_config", "project is no longer accessible")
		}
		for _, item := range data.Node.Items.Nodes {
			if !t.inScope(item) {
				continue
			}
			issue, err := t.normalize(ctx, item)
			if err != nil {
				if e, ok := err.(*Error); ok && e.Category == "tracker_response" && e.Message == "malformed project item" {
					if t.logger != nil {
						identifier := ""
						if item.Content.Number > 0 && strings.Contains(item.Content.Repository.NameWithOwner, "/") {
							identifier = fmt.Sprintf("%s#%d", item.Content.Repository.NameWithOwner, item.Content.Number)
						}
						t.logger.Warn("omitting malformed GitHub project item", "issue_id", item.ID, "issue_identifier", identifier)
					}
					continue
				}
				return nil, err
			}
			if wanted[githubNorm(issue.State)] && !seenIDs[issue.ID] {
				seenIDs[issue.ID] = true
				out = append(out, issue)
			}
		}
		next, more, err := githubNext(data.Node.Items.PageInfo, seen)
		if err != nil {
			return nil, err
		}
		if !more {
			return out, nil
		}
		cursor = next
	}
}
func (t *githubTracker) inScope(i githubItem) bool {
	return i.Type == "ProjectV2Item" && i.Project.ID == t.project && i.Content != nil && i.Content.Type == "Issue"
}
func (t *githubTracker) FetchIssuesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	out := []Issue{}
	unique := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	for start := 0; start < len(unique); start += 100 {
		end := start + 100
		if end > len(unique) {
			end = len(unique)
		}
		var data struct{ Nodes []*githubItem }
		err := t.request(ctx, `query($ids:[ID!]!){nodes(ids:$ids){`+`__typename ... on ProjectV2Item{`+githubItemFields+`}}}`, map[string]any{"ids": unique[start:end]}, &data)
		if err != nil {
			return nil, err
		}
		if len(data.Nodes) != end-start {
			return nil, githubError("tracker_response", "incomplete nodes response")
		}
		for n, item := range data.Nodes {
			if item == nil || !t.inScope(*item) {
				continue
			}
			if item.ID != unique[start+n] {
				return nil, githubError("tracker_response", "unexpected project item identity")
			}
			issue, err := t.normalize(ctx, *item)
			if err != nil {
				return nil, err
			}
			out = append(out, issue)
		}
	}
	return out, nil
}
func (t *githubTracker) normalize(ctx context.Context, item githubItem) (Issue, error) {
	i := item.Content
	state := ""
	values := item.FieldValues
	cursor := ""
	seen := map[string]bool{}
	for {
		for _, v := range values.Nodes {
			if v.Type == "ProjectV2ItemFieldSingleSelectValue" && v.Field.ID == t.fieldID && v.Name != nil {
				state = *v.Name
			}
		}
		next, more, err := githubNext(values.PageInfo, seen)
		if err != nil {
			return Issue{}, err
		}
		if !more {
			break
		}
		cursor = next
		var data struct {
			Node *struct {
				ID          string
				FieldValues githubConnection[githubValue]
			}
		}
		err = t.request(ctx, `query($id:ID!,$cursor:String){node(id:$id){... on ProjectV2Item{id fieldValues(first:100,after:$cursor){nodes{__typename ... on ProjectV2ItemFieldSingleSelectValue{name field{... on ProjectV2FieldCommon{id}}}} `+githubPage+`}}}}`, map[string]any{"id": item.ID, "cursor": cursor}, &data)
		if err != nil {
			return Issue{}, err
		}
		if data.Node == nil || data.Node.ID != item.ID {
			return Issue{}, githubError("tracker_response", "project item disappeared while paging")
		}
		values = data.Node.FieldValues
	}
	labels := []string{}
	labelSet := map[string]bool{}
	lc := i.Labels
	seen = map[string]bool{}
	for {
		for _, l := range lc.Nodes {
			s := githubNorm(l.Name)
			if s != "" && !labelSet[s] {
				labelSet[s] = true
				labels = append(labels, s)
			}
		}
		next, more, err := githubNext(lc.PageInfo, seen)
		if err != nil {
			return Issue{}, err
		}
		if !more {
			break
		}
		var data struct {
			Node *struct {
				ID     string
				Labels githubConnection[struct{ Name string }]
			}
		}
		err = t.request(ctx, `query($id:ID!,$cursor:String){node(id:$id){... on Issue{id labels(first:100,after:$cursor){nodes{name} `+githubPage+`}}}}`, map[string]any{"id": i.ID, "cursor": next}, &data)
		if err != nil {
			return Issue{}, err
		}
		if data.Node == nil || data.Node.ID != i.ID {
			return Issue{}, githubError("tracker_response", "issue disappeared while paging")
		}
		lc = data.Node.Labels
	}
	assignee := ""
	ac := i.Assignees
	seen = map[string]bool{}
	for {
		for _, a := range ac.Nodes {
			if a.ID != "" && (assignee == "" || a.ID < assignee) {
				assignee = a.ID
			}
		}
		next, more, err := githubNext(ac.PageInfo, seen)
		if err != nil {
			return Issue{}, err
		}
		if !more {
			break
		}
		var data struct {
			Node *struct {
				ID        string
				Assignees githubConnection[struct{ ID string }]
			}
		}
		err = t.request(ctx, `query($id:ID!,$cursor:String){node(id:$id){... on Issue{id assignees(first:100,after:$cursor){nodes{id} `+githubPage+`}}}}`, map[string]any{"id": i.ID, "cursor": next}, &data)
		if err != nil {
			return Issue{}, err
		}
		if data.Node == nil || data.Node.ID != i.ID {
			return Issue{}, githubError("tracker_response", "issue disappeared while paging")
		}
		ac = data.Node.Assignees
	}
	if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(i.ID) == "" || strings.TrimSpace(i.Title) == "" || strings.TrimSpace(state) == "" || i.Number <= 0 || !strings.Contains(i.Repository.NameWithOwner, "/") || item.IsArchived == nil || i.Locked == nil || (i.State != "OPEN" && i.State != "CLOSED") {
		return Issue{}, githubError("tracker_response", "malformed project item")
	}
	out := Issue{ID: item.ID, Identifier: fmt.Sprintf("%s#%d", i.Repository.NameWithOwner, i.Number), NativeRef: map[string]any{"project_id": t.project, "project_item_id": item.ID, "issue_id": i.ID, "repository": i.Repository.NameWithOwner, "issue_number": i.Number}, Title: i.Title, Description: i.Body, URL: i.URL, State: state, Labels: labels, BlockedBy: []Blocker{}, Dispatchable: !*item.IsArchived && i.State == "OPEN" && !*i.Locked, CreatedAt: githubTime(i.CreatedAt), UpdatedAt: githubTime(i.UpdatedAt)}
	if assignee != "" {
		out.AssigneeID = &assignee
	}
	return out, nil
}
func githubTime(s *string) *time.Time {
	if s == nil {
		return nil
	}
	t, e := time.Parse(time.RFC3339, *s)
	if e != nil {
		return nil
	}
	return &t
}
func (t *githubTracker) SecretEnvironmentNames() []string { return append([]string{}, t.secrets...) }
