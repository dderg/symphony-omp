package symphony

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a real child process, not an in-process runner mock.
func TestSpecAgentFixture(t *testing.T) {
	if os.Getenv("SYMPHONY_SPEC_AGENT") != "1" {
		return
	}
	logPath := os.Getenv("SYMPHONY_SPEC_LOG")
	record := func(v any) {
		f, e := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			os.Exit(31)
		}
		json.NewEncoder(f).Encode(v)
		f.Close()
	}
	cwd, _ := os.Getwd()
	secrets := []string{}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "SPEC_TRACKER_SECRET"} {
		if _, ok := os.LookupEnv(name); ok {
			secrets = append(secrets, name)
		}
	}
	record(map[string]any{"fixture": "launch", "cwd": cwd, "secret_names": secrets})
	encoder := json.NewEncoder(os.Stdout)
	send := func(v any) {
		if encoder.Encode(v) != nil {
			os.Exit(32)
		}
	}
	send(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 1048576, "maxReassembledFrameBytes": 67108864})
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1048576)
	promptID := ""
	for scanner.Scan() {
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(33)
		}
		record(frame)
		kind, _ := frame["type"].(string)
		id := frame["id"]
		respond := func(data any) {
			send(map[string]any{"type": "response", "id": id, "command": kind, "success": true, "data": data})
		}
		switch kind {
		case "negotiate_protocol":
			respond(map[string]any{"protocolVersion": 2})
		case "new_session":
			respond(map[string]any{"cancelled": false})
		case "get_state":
			respond(map[string]any{"sessionId": "spec-session", "isSettled": true})
		case "set_host_tools":
			respond(map[string]any{})
		case "prompt":
			promptID, _ = id.(string)
			respond(map[string]any{"agentInvoked": true})
			marker := os.Getenv("SYMPHONY_SPEC_FAILED")
			if _, e := os.Stat(marker); os.IsNotExist(e) {
				os.WriteFile(marker, []byte("first attempt failed"), 0600)
				send(map[string]any{"type": "prompt_result", "id": promptID, "status": "error", "error": "intentional fixture failure"})
			} else {
				send(map[string]any{"type": "host_tool_call", "id": "spec-tool", "toolCallId": "spec-model-tool", "toolName": "github_set_status", "arguments": map[string]any{"status": "Done"}})
			}
		case "host_tool_result":
			if frame["isError"] == true {
				os.Exit(34)
			}
			send(map[string]any{"type": "prompt_result", "id": promptID, "status": "completed", "sessionSettled": true})
		case "get_session_stats":
			respond(map[string]any{"tokens": map[string]any{"input": 11, "output": 7, "total": 18}})
		case "abort":
			respond(map[string]any{})
		default:
			respond(map[string]any{})
		}
	}
	os.Exit(0)
}

func TestSpecCLIMainTLSProcessSmoke(t *testing.T) {
	testSpecCLIProcessSmoke(t, "")
}

func testSpecCLIProcessSmoke(t *testing.T, standalone string) {
	if _, err := os.Stat("cmd/symphony/main.go"); err != nil {
		t.Fatal("actual CLI unavailable:", err)
	}
	dir := t.TempDir()
	binary := standalone
	if binary == "" {
		binary = filepath.Join(dir, "symphony")
		build := exec.Command("go", "test", "-c", "-o", binary, "./cmd/symphony")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building CLI main subprocess harness: %v\n%s", err, output)
		}
	}
	var mu sync.Mutex
	state := "Todo"
	mutations := 0
	graphql := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables map[string]any
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("invalid GraphQL request")
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Authorization") != "Bearer host-only-secret" {
			t.Error("host authentication missing")
		}
		mu.Lock()
		defer mu.Unlock()
		graphql++
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
				t.Error("incorrect board transition variables")
			}
			state = "Done"
			mutations++
			data = map[string]any{"updateProjectV2ItemFieldValue": map[string]any{"projectV2Item": map[string]any{"id": "ITEM"}}}
		default:
			t.Error("unexpected GraphQL query", req.Query)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	certPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "agent.jsonl")
	failedPath := filepath.Join(dir, "failed")
	hooksPath := filepath.Join(dir, "hooks")
	root := filepath.Join(dir, "workspaces")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	command := quote(fixtureExe) + " -test.run=^TestSpecAgentFixture$"
	yamlString := func(s string) string { raw, _ := json.Marshal(s); return string(raw) }
	text := fmt.Sprintf("---\ntracker:\n  kind: github_projects\n  provider:\n    project_id: PROJECT\n    endpoint: %s\n    api_key: $SPEC_TRACKER_SECRET\n  active_states: [Todo]\n  terminal_states: [Done]\npolling:\n  interval_ms: 100\nworkspace:\n  root: %s\nhooks:\n  after_run: %s\n  before_remove: %s\nagent:\n  max_turns: 1\n  max_retry_backoff_ms: 100\nomp:\n  command: %s\n  read_timeout_ms: 3000\n  turn_timeout_ms: 3000\n---\nSmoke {{ issue.identifier }}\n", server.URL, root, yamlString("printf 'after_run\\n' >> "+quote(hooksPath)), yamlString("printf 'before_remove\\n' >> "+quote(hooksPath)), yamlString(command))
	text = strings.Replace(text, "max_retry_backoff_ms: 100", "max_retry_backoff_ms: 1500", 1)
	if err = os.WriteFile(workflow, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestSpecCLIProcessMain$")
	cmd.Env = append(os.Environ(), "SPEC_CLI_PROCESS=1", "SPEC_CLI_CA_FILE="+certPath, "SPEC_CLI_WORKFLOW="+workflow, "SPEC_TRACKER_SECRET=host-only-secret", "GH_TOKEN=must-not-inherit", "GITHUB_TOKEN=must-not-inherit", "GH_ENTERPRISE_TOKEN=must-not-inherit", "GITHUB_ENTERPRISE_TOKEN=must-not-inherit", "SYMPHONY_SPEC_AGENT=1", "SYMPHONY_SPEC_LOG="+logPath, "SYMPHONY_SPEC_FAILED="+failedPath)
	if standalone != "" {
		cmd.Args = []string{binary, workflow}
		cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+certPath)
	}
	stderrPath := filepath.Join(dir, "cli.stderr")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stderrFile.Close()
	cmd.Stderr = stderrFile
	stderrText := func() string { raw, _ := os.ReadFile(stderrPath); return string(raw) }
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	defer func() {
		if !reaped {
			cmd.Process.Kill()
			<-done
		}
	}()
	deadline := time.Now().Add(12 * time.Second)
	complete := false
	reloaded := false
	for time.Now().Before(deadline) {
		if !reloaded {
			if _, e := os.Stat(failedPath); e == nil {
				if e = os.WriteFile(workflow, []byte(strings.Replace(text, "Smoke {{", "Reloaded {{", 1)), 0600); e != nil {
					t.Fatal(e)
				}
				reloaded = true
			}
		}
		hooks, _ := os.ReadFile(hooksPath)
		if strings.Contains(string(hooks), "before_remove") {
			complete = true
			break
		}
		select {
		case e := <-done:
			reaped = true
			t.Fatalf("CLI exited before cleanup: %v\n%s", e, stderrText())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !complete {
		t.Fatalf("no handoff cleanup observed\n%s", stderrText())
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		reaped = true
		if err != nil {
			t.Fatalf("signal shutdown: %v\n%s", err, stderrText())
		}
	case <-time.After(7 * time.Second):
		t.Fatal("signal shutdown hung")
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	launches, stats, results, reloadPrompts := 0, 0, 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var event map[string]any
		if json.Unmarshal(line, &event) != nil {
			t.Fatal("bad fixture log")
		}
		if event["fixture"] == "launch" {
			launches++
			expectedPath := filepath.Join(root, WorkspaceKey("o/r#3"))
			// The terminal workspace is gone, so canonicalize its trusted parent.
			physicalRoot, e := filepath.EvalSymlinks(root)
			if e != nil {
				t.Fatal(e)
			}
			expectedPath = filepath.Join(physicalRoot, filepath.Base(expectedPath))
			if event["cwd"] != expectedPath {
				t.Fatal("incorrect child cwd", event)
			}
			if len(event["secret_names"].([]any)) != 0 {
				t.Fatal("child inherited tracker secrets", event)
			}
		}
		switch event["type"] {
		case "prompt":
			if message, ok := event["message"].(string); ok && strings.Contains(message, "Reloaded o/r#3") {
				reloadPrompts++
			}
		case "get_session_stats":
			stats++
		case "host_tool_result":
			results++
			if event["id"] != "spec-tool" {
				t.Fatal("tool result correlation lost")
			}
		}
	}
	if launches < 2 || stats < 2 || results != 1 {
		t.Fatalf("missing retry/tool/stats proof launches=%d stats=%d results=%d\n%s", launches, stats, results, raw)
	}
	if !reloaded || reloadPrompts != 1 {
		t.Fatalf("reloaded template not observed by real child: %s", raw)
	}
	if _, err = os.Stat(filepath.Join(root, WorkspaceKey("o/r#3"))); !os.IsNotExist(err) {
		t.Fatal("terminal workspace not cleaned")
	}
	if strings.Contains(stderrText(), "host-only-secret") {
		t.Fatal("credential leaked in CLI logs")
	}
	mu.Lock()
	defer mu.Unlock()
	if mutations != 1 {
		t.Fatal("unexpected transition count", mutations)
	}
	t.Logf("CLI subprocess standalone=%t: %d launches, %d GraphQL requests, %d transition, %d correlated tool result, %d stats refresh, %d reloaded prompt; terminal cleanup and SIGTERM verified", standalone != "", launches, graphql, mutations, results, stats, reloadPrompts)
}

func TestSpecStrictTemplateBoundaries(t *testing.T) {
	issue := Issue{ID: "ITEM", Identifier: "o/r#3", Title: "Work", State: "Todo", Labels: []string{}, BlockedBy: []Blocker{}}
	for _, source := range []string{
		`{{ issue.priority.missing }}`,
		`{{ forloop.index }}`,
		`{{ issue["missing"] }}`,
		`{% if unknown %}unreachable{% endif %}`,
		`{% for label in issue.labels %}{{ missing }}{% endfor %}`,
		`{% if false %}{% assign branch_only = 1 %}{% endif %}{{ branch_only }}`,
	} {
		t.Run(source, func(t *testing.T) {
			if result, err := RenderPrompt(&Workflow{Prompt: source}, issue, nil); err == nil {
				t.Fatalf("unknown reference rendered without error: %q", result)
			}
		})
	}
	for _, source := range []string{
		`{{ issue.priority }}{{ issue.description }}{{ attempt }}`,
		`{% for label in issue.labels %}{{ forloop.index }}{{ label }}{% endfor %}`,
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := RenderPrompt(&Workflow{Prompt: source}, issue, nil); err != nil {
				t.Fatal("legitimate nullable/scoped loop rejected:", err)
			}
		})
	}
}

type specSnapshotTracker struct{ issue Issue }

func (t *specSnapshotTracker) FetchIssuesByStates(_ context.Context, states []string) ([]Issue, error) {
	if containsState(states, t.issue.State) {
		return []Issue{t.issue}, nil
	}
	return []Issue{}, nil
}
func (t *specSnapshotTracker) FetchIssuesByIDs(_ context.Context, ids []string) ([]Issue, error) {
	if len(ids) > 0 {
		return []Issue{t.issue}, nil
	}
	return []Issue{}, nil
}
func (*specSnapshotTracker) AgentToolSpecs() []ToolSpec       { return []ToolSpec{} }
func (*specSnapshotTracker) SecretEnvironmentNames() []string { return []string{} }
func (*specSnapshotTracker) ExecuteAgentTool(context.Context, string, json.RawMessage, Issue) ToolResult {
	return ToolResult{IsError: true}
}

func TestSpecFinalCumulativeStatisticsSnapshot(t *testing.T) {
	issue := Issue{ID: "ITEM", Identifier: "o/r#3", Title: "Work", State: "Todo", Dispatchable: true, Labels: []string{}, BlockedBy: []Blocker{}}
	tracker := &specSnapshotTracker{issue: issue}
	workflow := &Workflow{Config: Config{Tracker: TrackerConfig{ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}}, PollInterval: time.Hour, MaxConcurrent: 1, MaxTurns: 1, MaxBackoff: time.Hour, WorkspaceRoot: t.TempDir(), Hooks: Hooks{Timeout: time.Second}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	orchestrator := newOrchestrator(workflow, tracker, logger)
	orchestrator.workspaces = &WorkspaceManager{Config: orchestrator.currentConfig, Logger: logger}
	orchestrator.load = func(string) (*Workflow, error) { return workflow, nil }
	orchestrator.adapter = func(context.Context, TrackerConfig, *slog.Logger) (Tracker, error) { return tracker, nil }
	orchestrator.runner = func(_ context.Context, _ *Workflow, _ Tracker, _ *WorkspaceManager, _ Issue, _ *int, emit func(AgentEvent)) error {
		for range 2 {
			emit(AgentEvent{Event: "session_stats", NativeSessionID: "stats-session", Usage: &TokenUsage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18}})
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- orchestrator.Run(ctx) }()
	var observed Snapshot
	for ctx.Err() == nil {
		snapshot, err := orchestrator.Snapshot(ctx)
		if err == nil && len(snapshot.Retrying) > 0 {
			observed = snapshot
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if observed.Totals.InputTokens != 11 || observed.Totals.OutputTokens != 7 || observed.Totals.TotalTokens != 18 {
		t.Fatalf("final/repeated cumulative stats lost or double-counted: %#v", observed.Totals)
	}
}
