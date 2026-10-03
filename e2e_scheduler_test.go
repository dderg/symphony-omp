package symphony

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
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

// These scenarios go through the release CLI, real adapter, actor, filesystem,
// hooks and RPC processes; no scheduler/runner functions are replaced.
func TestStandaloneSchedulerPipeline(t *testing.T) {
	binary := os.Getenv("SYMPHONY_STANDALONE_BINARY")
	if binary == "" {
		t.Skip("standalone binary profile is not enabled")
	}
	for _, scenario := range []string{"capacity-reload-labels", "retry-slots", "backoff", "restart", "retry-root", "retry-root-follow", "provider-atomic", "startup-cleanup", "refresh-error", "blank-label"} {
		t.Run(scenario, func(t *testing.T) { standaloneScheduler(t, binary, scenario) })
	}
}

func standaloneScheduler(t *testing.T, binary, scenario string) {
	dir := t.TempDir()
	var mu sync.Mutex
	providerCalls := 0
	providerFailure := scenario == "provider-atomic"
	refreshFailure := false
	items := map[string]map[string]any{}
	for n, id := range []string{"old", "new", "other", "unroutable", "unlabeled"} {
		state := "Todo"
		if id == "other" {
			state = "In Progress"
		}
		item := githubTestItem(id, "PROJECT", state)
		content := item["content"].(map[string]any)
		content["number"] = n + 1
		content["createdAt"] = fmt.Sprintf("2026-01-%02dT00:00:00Z", n+1)
		if id == "unroutable" {
			item["isArchived"] = true
		}
		if id == "unlabeled" {
			content["labels"] = map[string]any{"nodes": []any{}, "pageInfo": githubTestPage(false, "")}
		}
		items[id] = item
	}
	if scenario == "startup-cleanup" {
		items["old"]["fieldValues"] = githubTestItem("old", "PROJECT", "Done")["fieldValues"]
	}
	single := scenario != "capacity-reload-labels" && scenario != "retry-slots"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer scheduler-synthetic-secret" {
			t.Error("host credential missing")
			w.WriteHeader(401)
			return
		}
		var req struct {
			Query     string
			Variables map[string]any
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		providerCalls++
		var data any
		switch {
		case strings.Contains(req.Query, "fields(first:"):
			data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "fields": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2SingleSelectField", "id": "STATUS", "name": "Status", "options": []any{map[string]any{"id": "T", "name": "Todo"}, map[string]any{"id": "P", "name": "In Progress"}, map[string]any{"id": "D", "name": "Done"}, map[string]any{"id": "R", "name": "Human Review"}}}}, "pageInfo": githubTestPage(false, "")}}}
		case strings.Contains(req.Query, "items(first:"):
			nodes := []any{}
			// Deliberately reverse chronological order: adapter order is not scheduler priority.
			for _, id := range []string{"unlabeled", "unroutable", "other", "new", "old"} {
				if !single || id == "old" {
					nodes = append(nodes, items[id])
				}
			}
			if providerFailure && req.Variables["cursor"] != nil {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "synthetic late-page failure"}}})
				return
			}
			data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": nodes, "pageInfo": githubTestPage(providerFailure, "next")}}}
		case strings.Contains(req.Query, "nodes(ids:"):
			if refreshFailure {
				_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": "synthetic refresh failure"}}})
				return
			}
			nodes := []any{}
			for _, id := range req.Variables["ids"].([]any) {
				nodes = append(nodes, items[id.(string)])
			}
			data = map[string]any{"nodes": nodes}
		default:
			t.Errorf("unexpected query: %s", req.Query)
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	scalar := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	root := filepath.Join(dir, "workspaces")
	workflow := filepath.Join(dir, "WORKFLOW.md")
	record := filepath.Join(dir, "peer.jsonl")
	hooks := filepath.Join(dir, "hooks")
	mode := "ack-only"
	capMS := 100
	concurrent := 2
	byState := "    ' ToDo ': 1\n    'In Progress': 1\n    invalid: -1"
	if scenario == "retry-slots" {
		mode = "slot-failure"
		concurrent = 1
		byState = "{}"
	}
	if scenario == "backoff" {
		mode = "turn-error"
		capMS = 25000
	}
	if scenario == "restart" || scenario == "retry-root" || scenario == "retry-root-follow" {
		mode = "normal"
	}
	text := func(limit int, workspace string) string {
		override := byState
		if override != "{}" {
			override = "\n" + override
		}
		return fmt.Sprintf("---\ntracker:\n  kind: github_projects\n  provider:\n    project_id: PROJECT\n    endpoint: %s\n    api_key: $RUNTIME_TRACKER_SECRET\n  active_states: [Todo, In Progress]\n  terminal_states: [Done]\n  required_labels: [' BUG ']\npolling:\n  interval_ms: 40\nworkspace:\n  root: %s\nhooks:\n  after_create: %s\n  before_run: %s\n  after_run: %s\n  before_remove: %s\nagent:\n  max_turns: 1\n  max_concurrent_agents: %d\n  max_concurrent_agents_by_state: %s\n  max_retry_backoff_ms: %d\nomp:\n  command: %s\n  read_timeout_ms: 1500\n  turn_timeout_ms: 60000\n  stall_timeout_ms: 0\n---\nORIGINAL {{ issue.identifier }} attempt={{ attempt }}\n", server.URL, workspace, scalar("printf 'created %s\\n' \"$PWD\" >> "+quote(hooks)), scalar("printf 'before %s\\n' \"$PWD\" >> "+quote(hooks)), scalar("printf 'after %s\\n' \"$PWD\" >> "+quote(hooks)), scalar("printf 'removed %s\\n' \"$PWD\" >> "+quote(hooks)), limit, override, capMS, scalar(quote(exe)+" -test.run=^TestRuntimeProtocolPeer$"))
	}
	writeWorkflow := func(s string) {
		if err := os.WriteFile(workflow, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeWorkflow(text(concurrent, root))
	if scenario == "blank-label" {
		writeWorkflow(strings.Replace(text(concurrent, root), "required_labels: [' BUG ']", "required_labels: [' ']", 1))
	}
	if scenario == "startup-cleanup" {
		if err = os.MkdirAll(filepath.Join(root, WorkspaceKey("o/r#1")), 0700); err != nil {
			t.Fatal(err)
		}
		source := text(concurrent, root)
		source = strings.Replace(source, scalar("printf 'removed %s\\n' \"$PWD\" >> "+quote(hooks)), scalar("printf 'removed %s\\n' \"$PWD\" >> "+quote(hooks)+"; sleep 60"), 1)
		writeWorkflow(source)
	}
	readFile := func(path string) string { b, _ := os.ReadFile(path); return string(b) }
	launch := func() (*exec.Cmd, chan error, string) {
		logPath := filepath.Join(dir, fmt.Sprintf("stderr-%d", time.Now().UnixNano()))
		f, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		cmd := exec.Command(binary, workflow)
		cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "SSL_CERT_FILE="+ca, "RUNTIME_TRACKER_SECRET=scheduler-synthetic-secret", "SYMPHONY_RUNTIME_PEER="+mode, "SYMPHONY_RUNTIME_RECORD="+record)
		cmd.Stderr = f
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, done, logPath
	}
	cmd, done, logs := launch()
	reaped := false
	defer func() {
		if !reaped {
			cmd.Process.Kill()
			<-done
		}
	}()
	waitUntil := func(check func() bool) {
		deadline := time.Now().Add(35 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			select {
			case err := <-done:
				reaped = true
				t.Fatalf("unexpected host exit %v\n%s", err, readFile(logs))
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("scenario deadline\n%s\n%s", readFile(logs), readFile(record))
	}
	launches := func() []string {
		out := []string{}
		for _, line := range strings.Split(readFile(record), "\n") {
			var e map[string]any
			if json.Unmarshal([]byte(line), &e) == nil {
				if path, ok := e["launch"].(string); ok {
					out = append(out, path)
				}
			}
		}
		return out
	}
	setState := func(id, state string) {
		mu.Lock()
		defer mu.Unlock()
		items[id]["fieldValues"] = githubTestItem(id, "PROJECT", state)["fieldValues"]
	}
	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			reaped = true
			if err != nil {
				t.Fatalf("shutdown %v\n%s", err, readFile(logs))
			}
		case <-time.After(8 * time.Second):
			t.Fatal("shutdown timeout")
		}
	}
	observedPolls := func() {
		mu.Lock()
		target := providerCalls + 6
		mu.Unlock()
		waitUntil(func() bool { mu.Lock(); defer mu.Unlock(); return providerCalls >= target })
	}
	switch scenario {
	case "blank-label":
		observedPolls()
		if len(launches()) != 0 {
			t.Fatal("blank required label matched an issue")
		}
	case "refresh-error":
		waitUntil(func() bool { return len(launches()) == 1 })
		mu.Lock()
		refreshFailure = true
		mu.Unlock()
		waitUntil(func() bool { return strings.Contains(readFile(logs), "tracker_response") })
		observedPolls()
		if len(launches()) != 1 || strings.Contains(readFile(hooks), "after ") {
			t.Fatal("provider refresh failure stopped or duplicated active worker")
		}
		mu.Lock()
		refreshFailure = false
		mu.Unlock()
		setState("old", "Human Review")
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "after ") })
	case "startup-cleanup":
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "removed ") })
		stop()
		if _, err = os.Stat(filepath.Join(root, WorkspaceKey("o/r#1"))); !os.IsNotExist(err) {
			t.Fatal("startup terminal cleanup did not complete", err)
		}
		if len(launches()) != 0 {
			t.Fatal("terminal startup issue dispatched")
		}
		t.Log("standalone startup before_remove interrupted by SIGTERM, workspace removed, clean exit")
		return
	case "retry-root-follow":
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "after ") })
		replacement := filepath.Join(dir, "new-root")
		writeWorkflow(text(concurrent, replacement))
		waitUntil(func() bool { return len(launches()) == 2 })
		if launches()[1] != filepath.Join(replacement, WorkspaceKey("o/r#1")) {
			t.Fatal("future attempt did not use reloaded root", launches())
		}
		if _, err = os.Stat(filepath.Join(root, WorkspaceKey("o/r#1"))); err != nil {
			t.Fatal("root cutover prematurely destroyed local work")
		}
		setState("old", "Done")
		waitUntil(func() bool {
			_, oldErr := os.Stat(filepath.Join(root, WorkspaceKey("o/r#1")))
			_, newErr := os.Stat(filepath.Join(replacement, WorkspaceKey("o/r#1")))
			return os.IsNotExist(oldErr) && os.IsNotExist(newErr)
		})
	case "provider-atomic":
		waitUntil(func() bool { return strings.Contains(readFile(logs), "tracker_response") })
		observedPolls()
		if len(launches()) != 0 {
			t.Fatal("partial provider pages caused dispatch")
		}
		mu.Lock()
		providerFailure = false
		mu.Unlock()
		waitUntil(func() bool { return len(launches()) == 1 })
	case "capacity-reload-labels":
		waitUntil(func() bool { return len(launches()) == 2 })
		paths := launches()
		expected := map[string]bool{filepath.Join(root, WorkspaceKey("o/r#1")): true, filepath.Join(root, WorkspaceKey("o/r#3")): true}
		for _, path := range paths {
			if !expected[path] {
				t.Fatalf("dispatch order/routing wrong %v", paths)
			}
		}
		writeWorkflow("---\ninvalid: [\n---\n")
		waitUntil(func() bool { return strings.Contains(readFile(logs), "workflow_parse_error") })
		setState("other", "Human Review")
		waitUntil(func() bool {
			return strings.Contains(readFile(hooks), "after "+filepath.Join(root, WorkspaceKey("o/r#3")))
		})
		observedPolls()
		if len(launches()) != 2 {
			t.Fatal("invalid reload dispatched new work")
		}
		writeWorkflow(text(1, root))
		observedPolls()
		if len(launches()) != 2 {
			t.Fatal("reloaded global capacity ignored")
		}
		writeWorkflow(text(2, root))
		// Active-state refresh releases Todo per-state capacity without ending old worker.
		setState("old", "In Progress")
		waitUntil(func() bool { return len(launches()) == 3 })
		if launches()[2] != filepath.Join(root, WorkspaceKey("o/r#2")) {
			t.Fatal("wrong newly eligible dispatch", launches())
		}
		observedPolls()
		if len(launches()) != 3 {
			t.Fatal("claims/global/per-state/routing let duplicates run")
		}
		// Required-label removal stops a real active worker without deleting workspace.
		mu.Lock()
		items["new"]["content"].(map[string]any)["labels"] = map[string]any{"nodes": []any{}, "pageInfo": githubTestPage(false, "")}
		mu.Unlock()
		waitUntil(func() bool {
			return strings.Contains(readFile(hooks), "after "+filepath.Join(root, WorkspaceKey("o/r#2")))
		})
		if _, err = os.Stat(filepath.Join(root, WorkspaceKey("o/r#2"))); err != nil {
			t.Fatal("label reconciliation destroyed workspace")
		}
	case "retry-slots":
		waitUntil(func() bool { return strings.Contains(readFile(logs), "no available orchestrator slots") })
		if len(launches()) != 2 {
			t.Fatal("retry claim allowed duplicate launch", launches())
		}
		setState("new", "Human Review")
		setState("other", "Human Review")
		waitUntil(func() bool { return len(launches()) >= 3 })
		if launches()[2] != filepath.Join(root, WorkspaceKey("o/r#1")) {
			t.Fatal("queued retry did not reclaim available slot", launches())
		}
	case "backoff":
		waitUntil(func() bool { return len(launches()) >= 3 && strings.Contains(readFile(logs), `"delay_ms":25000`) })
		attempts := []int{}
		delays := []int{}
		for _, line := range strings.Split(readFile(logs), "\n") {
			var e map[string]any
			if json.Unmarshal([]byte(line), &e) == nil && e["outcome"] == "retrying" {
				attempts = append(attempts, int(e["attempt"].(float64)))
				delays = append(delays, int(e["delay_ms"].(float64)))
			}
		}
		if len(attempts) < 3 || attempts[0] != 1 || attempts[1] != 2 || attempts[2] != 3 || delays[0] != 10000 || delays[1] != 20000 || delays[2] != 25000 {
			t.Fatal("10s exponential/capped backoff wrong", attempts, delays)
		}
		if !strings.Contains(readFile(record), "attempt=1") || !strings.Contains(readFile(record), "attempt=2") {
			t.Fatal("retry template attempt missing")
		}
	case "restart":
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "after ") })
		stop()
		if err = os.WriteFile(filepath.Join(root, WorkspaceKey("o/r#1"), "preserved"), []byte("local work"), 0600); err != nil {
			t.Fatal(err)
		}
		cmd, done, logs = launch()
		reaped = false
		waitUntil(func() bool { return len(launches()) == 2 })
		if strings.Count(readFile(hooks), "created ") != 1 {
			t.Fatal("restart ran after_create again")
		}
		if data, err := os.ReadFile(filepath.Join(root, WorkspaceKey("o/r#1"), "preserved")); err != nil || string(data) != "local work" {
			t.Fatal("restart destroyed reused workspace", err)
		}
	case "retry-root":
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "after ") })
		replacement := filepath.Join(dir, "new-root")
		writeWorkflow(text(concurrent, replacement))
		setState("old", "Done")
		waitUntil(func() bool { _, err := os.Stat(filepath.Join(root, WorkspaceKey("o/r#1"))); return os.IsNotExist(err) })
		if !strings.Contains(readFile(hooks), "removed "+filepath.Join(root, WorkspaceKey("o/r#1"))) {
			t.Fatal("terminal retry cleanup not bound to old root")
		}
	}
	stop()
	if strings.Contains(readFile(logs), "scheduler-synthetic-secret") {
		t.Fatal("credential leaked")
	}
	t.Logf("standalone scheduler %s: %d real agent process launches, hook effects and clean shutdown", scenario, len(launches()))
}
