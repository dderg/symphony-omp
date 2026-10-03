package symphony

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// These tests exercise actual authenticated HTTPS requests, not adapter wiring.
func TestExternalGitHubConnectionFailuresAreAtomic(t *testing.T) {
	for _, connection := range []string{"items", "fieldValues", "labels", "assignees"} {
		for _, failure := range []string{"repeated_cursor", "late_error", "disappeared"} {
			t.Run(connection+"/"+failure, func(t *testing.T) {
				pages := 0
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
						t.Error("missing host credential")
					}
					if strings.Contains(req.Query, "fields(first:") {
						json.NewEncoder(w).Encode(map[string]any{"data": githubTestFields()})
						return
					}
					item := githubTestItem("ITEM", "PROJECT", "Todo")
					page := map[string]any{"nodes": []any{item}, "pageInfo": githubTestPage(true, "NEXT")}
					nested := strings.Contains(req.Query, connection+"(first:100,after:")
					if connection == "items" || nested {
						pages++
						if req.Variables["cursor"] != nil {
							if req.Variables["cursor"] != "NEXT" {
								t.Errorf("wrong continuation %v", req.Variables)
							}
							if failure == "late_error" {
								fmt.Fprint(w, `{"data":null,"errors":[{"type":"INTERNAL","message":"secret-token"}]}`)
								return
							}
							if failure == "disappeared" {
								fmt.Fprint(w, `{"data":{"node":null}}`)
								return
							}
						}
					}
					var data any
					if connection == "items" {
						data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": page}}
					} else if nested {
						id := "ISSUE-ITEM"
						nodes := []any{}
						if connection == "fieldValues" {
							id = "ITEM"
						}
						data = map[string]any{"node": map[string]any{"id": id, connection: map[string]any{"nodes": nodes, "pageInfo": githubTestPage(true, "NEXT")}}}
					} else {
						target := item
						if connection != "fieldValues" {
							target = item["content"].(map[string]any)
						}
						target[connection].(map[string]any)["pageInfo"] = githubTestPage(true, "NEXT")
						data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": []any{item}, "pageInfo": githubTestPage(false, "")}}}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer server.Close()
				tracker, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
				if err != nil {
					t.Fatal(err)
				}
				out, err := tracker.FetchIssuesByStates(context.Background(), []string{"Todo"})
				if err == nil || out != nil || pages < 1 || strings.Contains(err.Error(), "secret-token") {
					t.Fatalf("partial/unsafe result: %v %v pages=%d", out, err, pages)
				}
			})
		}
	}
}

func TestExternalGitHubRedirectAndRateLimitIsolation(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		t.Error("redirect destination contacted")
	}))
	defer destination.Close()
	for _, mode := range []string{"redirect", "primary_rate", "secondary_rate", "graphql_rate", "trailing"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					w.Header().Set("Location", destination.URL)
					w.WriteHeader(307)
				case "primary_rate":
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.WriteHeader(403)
				case "secondary_rate":
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(403)
				case "graphql_rate":
					fmt.Fprint(w, `{"errors":[{"extensions":{"code":"RATE_LIMITED"},"message":"secret-token"}]}`)
				case "trailing":
					fmt.Fprint(w, `{"data":{}} {"secret":"secret-token"}`)
				}
			}))
			defer server.Close()
			_, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
			var categorized *Error
			if !errors.As(err, &categorized) || strings.Contains(err.Error(), "secret-token") {
				t.Fatal(err)
			}
			expected := "tracker_rate_limited"
			if mode == "redirect" {
				expected = "tracker_status"
			}
			if mode == "trailing" {
				expected = "tracker_response"
			}
			if categorized.Category != expected {
				t.Fatalf("got %s want %s", categorized.Category, expected)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("credential redirect isolation failed")
	}
}

func TestExternalGitHubConstructorFieldValidation(t *testing.T) {
	for _, mode := range []string{"inaccessible", "wrong_type", "missing", "duplicate", "nonselect", "empty_option", "duplicate_option", "unknown_state", "late_error", "repeated_cursor"} {
		t.Run(mode, func(t *testing.T) {
			pages := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				data := githubTestFields()
				node := data["node"].(map[string]any)
				fields := node["fields"].(map[string]any)
				field := fields["nodes"].([]any)[0].(map[string]any)
				switch mode {
				case "inaccessible":
					data["node"] = nil
				case "wrong_type":
					node["__typename"] = "Issue"
				case "missing":
					fields["nodes"] = []any{}
				case "duplicate":
					fields["nodes"] = []any{field, field}
				case "nonselect":
					field["__typename"] = "ProjectV2Field"
				case "empty_option":
					field["options"] = []any{map[string]any{"id": "", "name": "Todo"}}
				case "duplicate_option":
					field["options"] = []any{map[string]any{"id": "a", "name": "Todo"}, map[string]any{"id": "b", "name": " todo "}}
				case "late_error", "repeated_cursor":
					if pages == 1 {
						fields["nodes"] = []any{}
					}
					fields["pageInfo"] = githubTestPage(true, "NEXT")
					if mode == "late_error" && pages == 2 {
						fmt.Fprint(w, `{"errors":[{"message":"secret-token"}]}`)
						return
					}
				}
				json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			cfg := githubTestConfig(server.URL)
			if mode == "unknown_state" {
				cfg.ActiveStates = []string{"Unknown"}
			}
			_, err := NewGitHubProjects(context.Background(), cfg, server.Client(), githubTestLookup, nil)
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatal("invalid constructor accepted", err)
			}
		})
	}
}

// Opt-in: this consumes a real authenticated provider turn and never touches a real tracker.
func TestExternalActualOMPModelAndGitHubHandoff(t *testing.T) {
	if os.Getenv("SYMPHONY_ACTUAL_OMP") != "1" {
		t.Skip("set SYMPHONY_ACTUAL_OMP=1 to exercise installed authenticated omp")
	}
	var mu sync.Mutex
	state := "Todo"
	mutations := 0
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(random)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var data any
		switch {
		case strings.Contains(req.Query, "fields(first:"):
			data = githubTestFields()
		case strings.Contains(req.Query, "items(first:"):
			data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": []any{githubTestItem("ITEM", "PROJECT", state)}, "pageInfo": githubTestPage(false, "")}}}
		case strings.Contains(req.Query, "nodes(ids:"):
			data = map[string]any{"nodes": []any{githubTestItem("ITEM", "PROJECT", state)}}
		case strings.Contains(req.Query, "updateProjectV2ItemFieldValue"):
			if req.Variables["project"] != "PROJECT" || req.Variables["item"] != "ITEM" || req.Variables["field"] != "STATUS" || req.Variables["option"] != "OPT-DONE" {
				t.Error("unscoped mutation")
			}
			state = "Done"
			mutations++
			data = map[string]any{"updateProjectV2ItemFieldValue": map[string]any{"projectV2Item": map[string]any{"id": "ITEM"}}}
		default:
			t.Error("unexpected actual-model GraphQL operation")
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cfg := githubTestConfig(server.URL)
	tracker, err := NewGitHubProjects(ctx, cfg, server.Client(), func(name string) (string, bool) { return token, name == "CUSTOM_TOKEN" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := tracker.FetchIssuesByIDs(ctx, []string{"ITEM"})
	if err != nil || len(issues) != 1 {
		t.Fatal(issues, err)
	}
	root := t.TempDir()
	command := "omp --mode rpc --no-ui --no-session --no-extensions --no-skills --no-rules --no-lsp --no-title --tools write --approval-mode yolo --model openai-codex/gpt-6.1-sol --thinking low"
	if override := os.Getenv("SYMPHONY_ACTUAL_OMP_COMMAND"); override != "" {
		command = override
	}
	runtime := Config{Tracker: cfg, WorkspaceRoot: root, MaxTurns: 1, Command: command, ReadTimeout: 5 * time.Second, TurnTimeout: 2 * time.Minute, Hooks: Hooks{Timeout: time.Second}}
	workflow := &Workflow{Config: runtime, Prompt: "This is an isolated integration check. Do exactly two actions: use write to create proof.txt in the current workspace with exactly the text SYMPHONY_ACTUAL_OMP_OK followed by a newline. Then call github_set_status with status Done. Do not read other files, use other tools, or modify anything else. Finish with one short sentence."}
	if binary := os.Getenv("SYMPHONY_ACTUAL_OMP_BINARY"); binary != "" {
		testActualOMPStandalone(t, ctx, binary, server.Config.Handler, root, command, workflow.Prompt, token, &mu, &state, &mutations)
		return
	}
	manager := &WorkspaceManager{Config: func() Config { return runtime }}
	usage := int64(0)
	events := []string{}
	err = RunAttempt(ctx, workflow, tracker, manager, issues[0], nil, func(event AgentEvent) {
		events = append(events, event.Event)
		if event.Usage != nil {
			usage = event.Usage.TotalTokens
		}
	})
	if err != nil {
		t.Fatalf("actual omp run failed: %v events=%v", err, events)
	}
	proof, err := os.ReadFile(filepath.Join(root, WorkspaceKey(issues[0].Identifier), "proof.txt"))
	if err != nil || string(proof) != "SYMPHONY_ACTUAL_OMP_OK\n" {
		t.Fatalf("model file proof absent/wrong: %q %v", proof, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if mutations != 1 || state != "Done" || usage <= 0 {
		t.Fatalf("actual model handoff/stats: mutations=%d state=%s tokens=%d events=%v", mutations, state, usage, events)
	}
	t.Logf("Installed actual omp: model-written file, scoped GitHub handoff, positive cumulative tokens=%d; events=%v", usage, events)
}

func testActualOMPStandalone(t *testing.T, ctx context.Context, binary string, handler http.Handler, root, command, prompt, token string, mu *sync.Mutex, state *string, mutations *int) {
	// Only synthetic fixture data and its synthetic token cross this short-lived tunnel.
	local := httptest.NewServer(handler)
	defer local.Close()
	tunnel := exec.CommandContext(ctx, "cloudflared", "tunnel", "--url", local.URL, "--no-autoupdate")
	stderr, err := tunnel.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = tunnel.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tunnel.Process.Kill(); _ = tunnel.Wait() }()
	urls := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		pattern := regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
		delivered := false
		for scanner.Scan() {
			if url := pattern.FindString(scanner.Text()); url != "" && !delivered {
				urls <- url
				delivered = true
			}
		}
	}()
	endpoint := ""
	select {
	case endpoint = <-urls:
	case <-ctx.Done():
		t.Fatal("trusted synthetic tunnel startup timed out")
	}
	// URL advertisement precedes DNS/edge registration; admit CLI only after HTTPS serves the fixture.
	probe := &http.Client{Timeout: 5 * time.Second}
	// Do not seed macOS negative DNS caching while the brand-new public name is unpublished.
	publicURL, _ := url.Parse(endpoint)
	publicDNS := "https://dns.google/resolve?name=" + url.QueryEscape(publicURL.Hostname()) + "&type=A&edns_client_subnet=0.0.0.0/0"
	publication := time.NewTicker(time.Second)
	defer publication.Stop()
	lastDNS := "no public DNS response"
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, publicDNS, nil)
		response, dnsErr := probe.Do(request)
		published := false
		if dnsErr == nil {
			var answer struct {
				Status int
				Answer []struct {
					Type int
					Data string
				}
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&answer)
			response.Body.Close()
			lastDNS = fmt.Sprintf("HTTP=%d DNS_status=%d valid_json=%t", response.StatusCode, answer.Status, decodeErr == nil)
			if response.StatusCode == 200 && decodeErr == nil && answer.Status == 0 {
				for _, record := range answer.Answer {
					published = published || (record.Type == 1 && net.ParseIP(record.Data).To4() != nil)
				}
			}
		} else if ctx.Err() == nil {
			lastDNS = dnsErr.Error()
		}
		if published {
			break
		}
		select {
		case <-publication.C:
		case <-ctx.Done():
			t.Fatalf("synthetic tunnel DNS was never publicly published: %s", lastDNS)
		}
	}
	admission := time.NewTicker(250 * time.Millisecond)
	defer admission.Stop()
	lastFailure := "no HTTPS probe result"
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"query":"query($project:ID!){node(id:$project){__typename ... on ProjectV2 {fields(first:100){nodes{__typename ... on ProjectV2FieldCommon{id name} ... on ProjectV2SingleSelectField{options{id name}}} pageInfo{hasNextPage endCursor}}}}}","variables":{"project":"PROJECT"}}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response, requestErr := probe.Do(request)
		if requestErr == nil {
			var envelope struct{ Data map[string]any }
			decodeErr := json.NewDecoder(response.Body).Decode(&envelope)
			response.Body.Close()
			lastFailure = fmt.Sprintf("HTTP status=%d valid_json=%t", response.StatusCode, decodeErr == nil)
			if response.StatusCode == 200 && decodeErr == nil && envelope.Data["node"] != nil {
				break
			}
		} else {
			lastFailure = strings.ReplaceAll(requestErr.Error(), token, "[REDACTED]")
		}
		select {
		case <-admission.C:
		case <-ctx.Done():
			t.Fatalf("synthetic tunnel never became HTTPS-ready: %s", lastFailure)
		}
	}
	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	proofPath := filepath.Join(filepath.Dir(workflowPath), "archived-model-proof.txt")
	hook, _ := json.Marshal("cp proof.txt '" + strings.ReplaceAll(proofPath, "'", "'\"'\"'") + "'")
	quotedCommand, _ := json.Marshal(command)
	text := fmt.Sprintf("---\ntracker:\n  kind: github_projects\n  provider:\n    project_id: PROJECT\n    endpoint: %s\n    api_key: $CUSTOM_TOKEN\n  active_states: [Todo]\n  terminal_states: [Done]\npolling:\n  interval_ms: 30000\nworkspace:\n  root: %s\nhooks:\n  after_run: %s\nagent:\n  max_turns: 1\nomp:\n  command: %s\n  read_timeout_ms: 5000\n  turn_timeout_ms: 120000\n---\n%s\n", endpoint, root, hook, quotedCommand, prompt)
	if err = os.WriteFile(workflowPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	logsPath := filepath.Join(filepath.Dir(workflowPath), "cli.log")
	logs, err := os.Create(logsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	cli := exec.CommandContext(ctx, binary, workflowPath)
	cli.Env = append(os.Environ(), "CUSTOM_TOKEN="+token)
	cli.Stderr = logs
	if err = cli.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cli.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			_ = cli.Process.Kill()
			<-done
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err = <-done:
			stopped = true
			content, _ := os.ReadFile(logsPath)
			t.Fatalf("standalone exited before model handoff: %v logs=%s", err, strings.ReplaceAll(string(content), token, "[REDACTED]"))
		case <-ctx.Done():
			content, _ := os.ReadFile(logsPath)
			mu.Lock()
			summary := fmt.Sprintf("mutations=%d state=%s", *mutations, *state)
			mu.Unlock()
			t.Fatalf("actual standalone/model integration timed out %s logs=%s", summary, strings.ReplaceAll(string(content), token, "[REDACTED]"))
		case <-ticker.C:
			mu.Lock()
			complete := *mutations == 1 && *state == "Done"
			mu.Unlock()
			content, _ := os.ReadFile(logsPath)
			settled, completed := false, false
			for _, line := range strings.Split(string(content), "\n") {
				var record struct {
					IssueID                string `json:"issue_id"`
					Event, Outcome, Reason string
					Error                  json.RawMessage
				}
				if json.Unmarshal([]byte(line), &record) != nil || record.IssueID != "ITEM" {
					continue
				}
				settled = settled || record.Event == "turn_completed"
				completed = completed || (record.Outcome == "completed" && record.Reason == "" && string(record.Error) == "null")
			}
			if !complete || !settled || !completed {
				continue
			}
			proof, err := os.ReadFile(proofPath)
			if err != nil || string(proof) != "SYMPHONY_ACTUAL_OMP_OK\n" {
				t.Fatalf("actual standalone model proof: %q %v", proof, err)
			}
			_, workspaceErr := os.Stat(filepath.Join(root, WorkspaceKey("o/r#3")))
			if workspaceErr == nil {
				continue
			}
			if !os.IsNotExist(workspaceErr) {
				t.Fatal("terminal workspace cleanup check failed", workspaceErr)
			}
			if err = cli.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				stopped = true
				if err != nil {
					t.Fatal("standalone graceful shutdown failed", err)
				}
			case <-ctx.Done():
				t.Fatal("standalone shutdown timed out")
			}
			t.Log("Native standalone release + installed actual omp + authenticated model: exact archived file, real scoped GraphQL handoff through public-trust synthetic HTTPS tunnel, settled turn, completed worker, terminal workspace removal and clean SIGTERM")
			return
		}
	}
}

func TestExternalActualOMPHeadlessApproval(t *testing.T) {
	if os.Getenv("SYMPHONY_ACTUAL_OMP") != "1" {
		t.Skip("requires installed authenticated omp")
	}
	for _, policy := range []string{"default", "always-ask"} {
		t.Run(policy, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Query string }
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(400)
					return
				}
				data := githubTestFields()
				if strings.Contains(req.Query, "nodes(ids:") {
					data = map[string]any{"nodes": []any{githubTestItem("ITEM", "PROJECT", "Todo")}}
				}
				json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			tracker, err := NewGitHubProjects(ctx, githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			command := "omp --mode rpc --no-ui --no-session --no-extensions --no-skills --no-rules --no-lsp --no-title --tools write --model openai-codex/gpt-6.1-sol --thinking low"
			if policy != "default" {
				command += " --approval-mode " + policy
			}
			runtime := Config{Tracker: githubTestConfig(server.URL), WorkspaceRoot: root, MaxTurns: 1, Command: command, ReadTimeout: 30 * time.Second, TurnTimeout: 60 * time.Second, Hooks: Hooks{Timeout: time.Second}}
			issue := Issue{ID: "ITEM", Identifier: "o/r#3", Title: "Headless approval", State: "Todo", Dispatchable: true}
			workflow := &Workflow{Config: runtime, Prompt: "Attempt exactly one write tool call to create approval-proof.txt with text APPROVAL_PROBE. If the tool refuses approval, do not retry or use another tool. Report whether it succeeded or refused, then finish. Do not call GitHub tools."}
			manager := &WorkspaceManager{Config: func() Config { return runtime }}
			path, err := manager.Create(ctx, issue.Identifier)
			if err != nil {
				t.Fatal(err)
			}
			client, err := startRPC(ctx, runtime, path, tracker, issue, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.shutdown(false)
			if err = client.startup(); err != nil {
				t.Fatal(err)
			}
			if err = client.prompt(workflow.Prompt); err != nil {
				t.Fatal("headless approval did not settle", err)
			}
			messages, err := client.request("get_messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			var history struct {
				Messages []struct {
					Role, ToolName string
					IsError        bool
				}
			}
			raw, _ := json.Marshal(messages)
			if json.Unmarshal(raw, &history) != nil {
				t.Fatal("invalid real message history")
			}
			attempted, refused := false, false
			for _, message := range history.Messages {
				if message.Role == "toolResult" && message.ToolName == "write" {
					attempted = true
					refused = message.IsError
				}
			}
			if !attempted || (policy == "always-ask" && !refused) {
				t.Fatalf("actual tool evidence missing: attempted=%t refused=%t", attempted, refused)
			}
			_, statErr := os.Stat(filepath.Join(root, WorkspaceKey(issue.Identifier), "approval-proof.txt"))
			if policy == "always-ask" && !os.IsNotExist(statErr) {
				t.Fatalf("approval-required headless write did not fail closed: %v", statErr)
			}
			if policy == "default" {
				t.Logf("Installed host default headless policy settled; file_written=%t (host configuration applies; no approval override)", statErr == nil)
			} else {
				t.Log("Explicit always-ask actual model write refused; no file; prompt settled without interactive wait")
			}
		})
	}
}

func TestExternalActualOMPPendingHostToolCancellation(t *testing.T) {
	if os.Getenv("SYMPHONY_ACTUAL_OMP") != "1" {
		t.Skip("requires installed authenticated omp")
	}
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Query string }
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if strings.Contains(req.Query, "updateProjectV2ItemFieldValue") {
			close(entered)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		data := githubTestFields()
		if strings.Contains(req.Query, "nodes(ids:") {
			data = map[string]any{"nodes": []any{githubTestItem("ITEM", "PROJECT", "Todo")}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tracker, err := NewGitHubProjects(ctx, githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Command: "omp --mode rpc --no-ui --no-session --no-extensions --no-skills --no-rules --no-lsp --no-title --no-tools --approval-mode yolo --model openai-codex/gpt-6.1-sol --thinking low", ReadTimeout: 30 * time.Second, TurnTimeout: 60 * time.Second}
	client, err := startRPC(ctx, cfg, t.TempDir(), tracker, Issue{ID: "ITEM"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = client.shutdown(true)
		}
	}()
	if err = client.startup(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- client.prompt("Call github_set_status with status Done exactly once. Do nothing else.")
	}()
	select {
	case <-entered:
	case err = <-result:
		t.Fatalf("model finished before pending tool: %v", err)
	case <-ctx.Done():
		t.Fatal("model never called host tool")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("actual pending provider request not cancelled")
	}
	select {
	case err = <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prompt cancellation mismatch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("actual model prompt cancellation did not return")
	}
	err = client.shutdown(true)
	stopped = true
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Real model host call reached faithful provider; cancellation aborted its HTTP context and actual omp terminated within bounded shutdown. Native host_tool_cancel frame is covered separately by deterministic protocol peers.")
}

func TestExternalGitHubRequiredOptionalAndVisibility(t *testing.T) {
	for _, mode := range []string{"removed", "inaccessible", "pull_request", "blank_title", "blank_issue_id", "missing_archive", "missing_lock", "bad_state", "bad_number", "missing_status", "optional_nulls"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Query string }
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(400)
					return
				}
				if strings.Contains(req.Query, "fields(first:") {
					json.NewEncoder(w).Encode(map[string]any{"data": githubTestFields()})
					return
				}
				item := githubTestItem("ITEM", "PROJECT", "Todo")
				content := item["content"].(map[string]any)
				var node any = item
				switch mode {
				case "removed":
					node = nil
				case "inaccessible":
					item["content"] = nil
				case "pull_request":
					content["__typename"] = "PullRequest"
				case "blank_title":
					content["title"] = " "
				case "blank_issue_id":
					content["id"] = ""
				case "missing_archive":
					delete(item, "isArchived")
				case "missing_lock":
					delete(content, "locked")
				case "bad_state":
					content["state"] = "UNKNOWN"
				case "bad_number":
					content["number"] = 0
				case "missing_status":
					item["fieldValues"].(map[string]any)["nodes"] = []any{}
				case "optional_nulls":
					for _, key := range []string{"body", "url", "createdAt", "updatedAt"} {
						content[key] = nil
					}
					content["labels"].(map[string]any)["nodes"] = []any{}
					content["assignees"].(map[string]any)["nodes"] = []any{}
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": []any{node}}})
			}))
			defer server.Close()
			tracker, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, err := tracker.FetchIssuesByIDs(context.Background(), []string{"ITEM"})
			switch mode {
			case "removed", "inaccessible", "pull_request":
				if err != nil || len(out) != 0 {
					t.Fatal("out-of-scope item retained", out, err)
				}
			case "optional_nulls":
				if err != nil || len(out) != 1 || out[0].Description != nil || out[0].URL != nil || out[0].CreatedAt != nil || out[0].UpdatedAt != nil || out[0].AssigneeID != nil || out[0].Labels == nil {
					t.Fatal("optional fallback", out, err)
				}
			default:
				if err == nil || out != nil {
					t.Fatal("malformed required field accepted", out, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result := tracker.ExecuteAgentTool(ctx, "github_add_comment", json.RawMessage(`{"body":"synthetic"}`), Issue{ID: "ITEM"})
			if !result.IsError || !strings.Contains(result.Content[0].Text, `"category":"cancelled"`) {
				t.Fatal("tool cancellation not structured", result)
			}
		})
	}
}

func TestExternalGitHubMalformedMutationIsError(t *testing.T) {
	for _, payload := range []string{`null`, `{}`, `42`, `{"issue":{"id":"FOREIGN","state":"CLOSED"}}`, `{"projectV2Item":{"id":"FOREIGN"}}`, `{"commentEdge":{"node":null}}`} {
		for _, call := range []struct{ name, args, root string }{
			{"github_set_status", `{"status":"Done"}`, "updateProjectV2ItemFieldValue"},
			{"github_add_comment", `{"body":"synthetic"}`, "addComment"},
			{"github_set_issue_state", `{"state":"CLOSED"}`, "closeIssue"},
			{"github_set_issue_state", `{"state":"OPEN"}`, "reopenIssue"},
		} {
			t.Run(call.root+"/"+payload, func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct{ Query string }
					if json.NewDecoder(r.Body).Decode(&req) != nil {
						w.WriteHeader(400)
						return
					}
					if strings.HasPrefix(req.Query, "mutation") {
						fmt.Fprintf(w, `{"data":{"%s":%s}}`, call.root, payload)
						return
					}
					data := githubTestFields()
					if strings.Contains(req.Query, "nodes(ids:") {
						data = map[string]any{"nodes": []any{githubTestItem("ITEM", "PROJECT", "Todo")}}
					}
					json.NewEncoder(w).Encode(map[string]any{"data": data})
				}))
				defer server.Close()
				tracker, err := NewGitHubProjects(context.Background(), githubTestConfig(server.URL), server.Client(), githubTestLookup, nil)
				if err != nil {
					t.Fatal(err)
				}
				result := tracker.ExecuteAgentTool(context.Background(), call.name, json.RawMessage(call.args), Issue{ID: "ITEM"})
				if !result.IsError || !strings.Contains(result.Content[0].Text, `"category":"tracker_response"`) {
					t.Fatal("malformed mutation reported success", result)
				}
			})
		}
	}
}
