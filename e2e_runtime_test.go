package symphony

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Run with SYMPHONY_STANDALONE_BINARY on Linux: private CA trust is supplied
// through the platform's ordinary SSL_CERT_FILE mechanism, never a TLS bypass.
func TestStandaloneRuntimeEndToEnd(t *testing.T) {
	binary := os.Getenv("SYMPHONY_STANDALONE_BINARY")
	if binary == "" {
		t.Skip("set SYMPHONY_STANDALONE_BINARY to an unmodified Linux release binary")
	}
	if runtime.GOOS != "linux" {
		t.Fatal("private fixture CA profile requires native Linux certificate trust")
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	testSpecCLIProcessSmoke(t, absolute)
}

func TestStandaloneStartupFailures(t *testing.T) {
	binary := os.Getenv("SYMPHONY_STANDALONE_BINARY")
	if binary == "" {
		t.Skip("standalone binary profile is not enabled")
	}
	for _, tc := range []struct {
		args             []string
		source, category string
	}{
		{nil, "", "missing_workflow_file"}, {[]string{"missing.md"}, "", "missing_workflow_file"},
		{nil, "---\na: [\n---\n", "workflow_parse_error"}, {nil, "---\n[one,two]\n---\n", "workflow_front_matter_not_a_map"},
		{nil, "---\ntracker:\n  kind: nope\n---\n", "unsupported_tracker_kind"},
		{nil, "---\ntracker:\n  kind: github_projects\nhooks:\n  timeout_ms: 0\n---\n", "invalid_config"},
		{nil, "---\ntracker:\n  kind: github_projects\nagent:\n  max_turns: 0\n---\n", "invalid_config"},
		{nil, "---\ntracker:\n  kind: github_projects\npolling:\n  interval_ms: 0\n---\n", "invalid_config"},
	} {
		cmd := exec.Command(binary, tc.args...)
		cmd.Dir = t.TempDir()
		if tc.source != "" {
			if err := os.WriteFile(filepath.Join(cmd.Dir, "WORKFLOW.md"), []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
		}
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), tc.category) {
			t.Fatalf("startup %v: %v: %s; want %s", tc.args, err, output, tc.category)
		}
	}
}

// A peer is a separate executable process with real stdio, EOF, and process-group
// semantics. It drives transport and session behavior, not expected host results.
func TestRuntimeProtocolPeer(t *testing.T) {
	mode := os.Getenv("SYMPHONY_RUNTIME_PEER")
	if mode == "" {
		return
	}
	record := func(value any) {
		f, err := os.OpenFile(os.Getenv("SYMPHONY_RUNTIME_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(21)
		}
		_ = json.NewEncoder(f).Encode(value)
		_ = f.Close()
	}
	cwd, _ := os.Getwd()
	record(map[string]any{"launch": cwd, "pid": os.Getpid(), "pgid": syscall.Getpgrp()})
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "RUNTIME_TRACKER_SECRET"} {
		if _, exists := os.LookupEnv(name); exists {
			os.Exit(22)
		}
	}
	send := func(value any) {
		if json.NewEncoder(os.Stdout).Encode(value) != nil {
			os.Exit(23)
		}
	}
	send(map[string]any{"type": "ready", "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 8192, "maxReassembledFrameBytes": 16384})
	scan := bufio.NewScanner(os.Stdin)
	settled, prompts := true, 0
	promptID := ""
	for scan.Scan() {
		var m map[string]any
		if json.Unmarshal(scan.Bytes(), &m) != nil {
			os.Exit(24)
		}
		record(m)
		kind, _ := m["type"].(string)
		id, _ := m["id"].(string)
		response := func(data any) { send(rpcResponse(id, kind, data)) }
		result := func(status string, quiet bool) {
			send(map[string]any{"type": "prompt_result", "id": id, "status": status, "sessionSettled": quiet})
		}
		switch kind {
		case "negotiate_protocol":
			response(map[string]any{"protocolVersion": 2})
		case "new_session":
			response(map[string]any{"cancelled": mode == "cancel-startup"})
		case "get_state":
			if !settled && mode == "delayed-prompt-error" {
				send(map[string]any{"type": "response", "id": promptID, "command": "prompt", "success": false, "error": "delayed active prompt failure"})
			}
			response(map[string]any{"sessionId": fmt.Sprintf("runtime-%d", os.Getpid()), "isSettled": settled})
			if !settled {
				if mode == "background-error" {
					send(map[string]any{"type": "message_end", "message": map[string]any{"stopReason": "error", "errorMessage": "background failed"}})
				} else {
					time.Sleep(50 * time.Millisecond)
					settled = true
					send(map[string]any{"type": "session_settled"})
				}
			}
		case "set_host_tools":
			response(nil)
		case "get_session_stats":
			response(map[string]any{"tokens": map[string]int{"input": 11, "output": 7, "total": 19}})
		case "prompt":
			prompts++
			promptID = id
			if mode == "continuation" && prompts > 1 && strings.Contains(fmt.Sprint(m["message"]), "ORIGINAL") {
				os.Exit(27)
			}
			if mode == "admission-timeout" {
				continue
			}
			if mode == "local" {
				response(map[string]any{"agentInvoked": false})
				continue
			}
			response(map[string]any{"agentInvoked": true})
			switch mode {
			case "interleaved":
				send(map[string]any{"type": "response", "id": "unrelated", "command": "get_state", "success": false, "error": "not the active request"})
				send(map[string]any{"type": "prompt_result", "id": "unrelated", "status": "error"})
				send(map[string]any{"type": "turn_end"})
				result("completed", true)
			case "ack-only", "terminal", "inactive", "missing", "ineligible", "stall", "shutdown", "remove-failure", "remove-timeout", "shutdown-long", "remove-long-shutdown":
				continue
			case "slot-failure":
				if filepath.Base(cwd) == WorkspaceKey("o/r#1") {
					result("error", true)
				}
			case "eof":
				os.Exit(0)
			case "aborted":
				result("aborted", true)
			case "turn-error":
				result("error", true)
			case "dialog":
				send(map[string]any{"type": "extension_ui_request", "id": "dialog", "method": "confirm"})
			case "background-error", "background", "delayed-prompt-error":
				settled = false
				result("completed", false)
			case "bad-index":
				_, _ = os.Stdout.Write(append(rpcChunk("bad", 1, 2, 2, []byte("}")), '\n'))
			case "bad-length":
				_, _ = os.Stdout.Write(append(rpcChunk("bad", 0, 1, 3, []byte("{}")), '\n'))
			case "bad-utf8":
				_, _ = os.Stdout.Write(append(rpcChunk("bad", 0, 1, 1, []byte{255}), '\n'))
			case "chunk-interrupted":
				_, _ = os.Stdout.Write(append(rpcChunk("bad", 0, 2, 2, []byte("{")), '\n'))
				send(map[string]any{"type": "notice"})
			case "chunk-id", "chunk-count", "chunk-order", "chunk-eof":
				_, _ = os.Stdout.Write(append(rpcChunk("bad", 0, 2, 2, []byte("{")), '\n'))
				switch mode {
				case "chunk-id":
					_, _ = os.Stdout.Write(append(rpcChunk("other", 1, 2, 2, []byte("}")), '\n'))
				case "chunk-count":
					_, _ = os.Stdout.Write(append(rpcChunk("bad", 1, 3, 2, []byte("}")), '\n'))
				case "chunk-order":
					_, _ = os.Stdout.Write(append(rpcChunk("bad", 0, 2, 2, []byte("}")), '\n'))
				case "chunk-eof":
					os.Exit(0)
				}
			case "chunk-base64":
				send(map[string]any{"type": "rpc_chunk", "chunkId": "bad", "index": 0, "count": 1, "byteLength": 2, "data": "???"})
			case "logical-overflow":
				_, _ = os.Stdout.Write(append(rpcChunk("bad", 0, 1, 16385, []byte("{}")), '\n'))
			case "physical-overflow":
				_, _ = os.Stdout.Write([]byte(strings.Repeat(" ", 8192) + "\n"))
			case "chunks":
				raw := rpcJSON(map[string]any{"type": "prompt_result", "id": id, "status": "completed", "sessionSettled": true})
				for i, part := range [][]byte{raw[:10], raw[10:]} {
					_, _ = os.Stdout.Write(append(rpcChunk("good", i, 2, len(raw), part), '\n'))
				}
			case "chunks-silence":
				raw := rpcJSON(map[string]any{"type": "prompt_result", "id": id, "status": "completed", "sessionSettled": true})
				for i := range 4 {
					time.Sleep(200 * time.Millisecond)
					start, end := i*len(raw)/4, (i+1)*len(raw)/4
					_, _ = os.Stdout.Write(append(rpcChunk("slow", i, 4, len(raw), raw[start:end]), '\n'))
				}
			case "tool-errors":
				send(map[string]any{"type": "host_tool_call", "id": "unknown", "toolName": "not_a_tool", "arguments": map[string]any{}})
				send(map[string]any{"type": "host_tool_call", "id": "invalid", "toolName": "github_set_status", "arguments": []any{}})
			case "tool-cancel":
				_ = os.WriteFile(filepath.Join(filepath.Dir(os.Getenv("SYMPHONY_RUNTIME_RECORD")), "host-call"), nil, 0600)
				send(map[string]any{"type": "host_tool_call", "id": "cancel-me", "toolName": "github_get_issue", "arguments": map[string]any{}})
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(filepath.Join(filepath.Dir(os.Getenv("SYMPHONY_RUNTIME_RECORD")), "provider-started")); err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
				send(map[string]any{"type": "host_tool_cancel", "targetId": "cancel-me"})
			case "stderr":
				fmt.Fprintln(os.Stderr, "not JSON: diagnostic only")
				result("completed", true)
			default:
				result("completed", true)
			}
		case "host_tool_result":
			if m["isError"] != true {
				os.Exit(25)
			}
			if id == "cancel-me" {
				_ = os.Remove(filepath.Join(filepath.Dir(os.Getenv("SYMPHONY_RUNTIME_RECORD")), "host-call"))
			}
			if id == "invalid" || id == "cancel-me" {
				send(map[string]any{"type": "prompt_result", "id": promptID, "status": "completed", "sessionSettled": true})
			}
		case "abort":
			record(map[string]any{"aborted": true})
			os.Exit(0)
		case "extension_ui_response":
			if m["cancelled"] != true {
				os.Exit(26)
			}
		}
	}
	os.Exit(0)
}

func TestStandaloneAdversarialScenarios(t *testing.T) {
	binary := os.Getenv("SYMPHONY_STANDALONE_BINARY")
	if binary == "" {
		t.Skip("standalone binary profile is not enabled")
	}
	cases := []struct{ mode, category string }{
		{"local", ""}, {"chunks", ""}, {"background", ""}, {"stderr", ""}, {"tool-errors", ""}, {"continuation", ""},
		{"ack-only", "turn_timeout"}, {"admission-timeout", "response_timeout"},
		{"background-error", "turn_failed"}, {"turn-error", "turn_failed"}, {"aborted", "turn_cancelled"},
		{"dialog", "turn_input_required"}, {"cancel-startup", "startup_failed"}, {"eof", "port_exit"},
		{"bad-index", "protocol_error"}, {"bad-length", "protocol_error"}, {"bad-utf8", "protocol_error"},
		{"chunk-interrupted", "protocol_error"}, {"logical-overflow", "protocol_error"}, {"physical-overflow", "protocol_error"},
		{"chunk-id", "protocol_error"}, {"chunk-count", "protocol_error"}, {"chunk-order", "protocol_error"}, {"chunk-base64", "protocol_error"}, {"chunk-eof", "protocol_error"},
		{"terminal", "terminal"}, {"inactive", "ineligible"}, {"missing", "missing"}, {"ineligible", "ineligible"}, {"stall", "stalled"}, {"shutdown", "shutdown"},
		{"strict-template", "template_render_error"}, {"create-failure", "after_create"}, {"before-failure", "before_run"},
		{"before-timeout", "before_run"}, {"after-failure", ""}, {"after-timeout", ""}, {"remove-failure", "terminal"}, {"remove-timeout", "terminal"}, {"after-long-shutdown", "shutdown"},
		{"defaults", ""}, {"env-path", ""}, {"tilde-path", ""}, {"relative-path", ""}, {"null-template", ""},
		{"unknown-filter", "template_render_error"}, {"inactive-template", "template_render_error"},
		{"symlink", "invalid_workspace_cwd"}, {"non-directory", "invalid_workspace_cwd"}, {"cwd-override", "invalid_workspace_cwd"},
		{"log-sink-failure", ""},
		{"remove-long-shutdown", "terminal"}, {"shutdown-long", "shutdown"},
		{"snapshot-api", ""},
		{"interleaved", ""}, {"chunks-silence", ""},
		{"create-timeout", "after_create"}, {"tool-cancel", ""}, {"outbound-overflow", "protocol_error"}, {"root-alias", ""},
		{"delayed-prompt-error", "response_error"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) { runtimeScenario(t, binary, tc.mode, tc.category) })
	}
}

func runtimeScenario(t *testing.T, binary, mode, category string) {
	t.Helper()
	dir := t.TempDir()
	var mu sync.Mutex
	state, visible, dispatchable := "Todo", true, true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runtime-synthetic-secret" {
			t.Error("tracker host authentication missing")
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
		if mode == "tool-cancel" && strings.Contains(req.Query, "nodes(ids:") {
			if _, err := os.Stat(filepath.Join(dir, "host-call")); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "provider-started"), nil, 0600)
				<-r.Context().Done()
				_ = os.WriteFile(filepath.Join(dir, "provider-canceled"), nil, 0600)
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		var data any
		switch {
		case strings.Contains(req.Query, "fields(first:"):
			fields := githubTestFields()
			field := fields["node"].(map[string]any)["fields"].(map[string]any)["nodes"].([]any)[0].(map[string]any)
			field["options"] = append(field["options"].([]any), map[string]any{"id": "OPT-REVIEW", "name": "Human Review"}, map[string]any{"id": "OPT-PROGRESS", "name": "In Progress"})
			data = fields
		case strings.Contains(req.Query, "items(first:"), strings.Contains(req.Query, "nodes(ids:"):
			items := []any{}
			if visible {
				item := githubTestItem("ITEM", "PROJECT", state)
				if !dispatchable {
					item["isArchived"] = true
				}
				items = append(items, item)
			}
			if strings.Contains(req.Query, "items(first:") {
				data = map[string]any{"node": map[string]any{"__typename": "ProjectV2", "items": map[string]any{"nodes": items, "pageInfo": githubTestPage(false, "")}}}
			} else {
				if !visible {
					items = []any{nil}
				}
				data = map[string]any{"nodes": items}
			}
		default:
			t.Errorf("unexpected provider operation %s", req.Query)
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
	record := filepath.Join(dir, "peer.jsonl")
	hooks := filepath.Join(dir, "hooks")
	root := filepath.Join(dir, "workspaces")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	scalar := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	hook := func(name string) string { return scalar("printf '" + name + "\\n' >> " + quote(hooks)) }
	command := quote(exe) + " -test.run=^TestRuntimeProtocolPeer$"
	stall := 0
	if mode == "stall" {
		stall = 150
	}
	source := fmt.Sprintf("---\ntracker:\n  kind: github_projects\n  provider:\n    project_id: PROJECT\n    endpoint: %s\n    api_key: $RUNTIME_TRACKER_SECRET\n  active_states: [Todo]\n  terminal_states: [Done]\npolling:\n  interval_ms: 40\nworkspace:\n  root: %s\nhooks:\n  after_create: %s\n  before_run: %s\n  after_run: %s\n  before_remove: %s\nagent:\n  max_turns: 1\n  max_retry_backoff_ms: 10000\nomp:\n  command: %s\n  read_timeout_ms: 150\n  turn_timeout_ms: 400\n  stall_timeout_ms: %d\n---\nORIGINAL {{ issue.identifier }}\n", server.URL, root, hook("created"), hook("before"), hook("after"), hook("removed"), scalar(command), stall)
	workflow := filepath.Join(dir, "WORKFLOW.md")
	if mode == "continuation" {
		source = strings.Replace(source, "max_turns: 1", "max_turns: 2", 1)
	}
	source = strings.Replace(source, "read_timeout_ms: 150", "read_timeout_ms: 2000", 1)
	if mode == "admission-timeout" {
		source = strings.Replace(source, "read_timeout_ms: 2000", "read_timeout_ms: 700", 1)
		source = strings.Replace(source, "turn_timeout_ms: 400", "turn_timeout_ms: 2000", 1)
	}
	changeHook := func(name, script string) {
		source = strings.Replace(source, "  "+name+": "+hook(map[string]string{"after_create": "created", "before_run": "before", "after_run": "after", "before_remove": "removed"}[name]), "  "+name+": "+scalar(script), 1)
	}
	switch mode {
	case "strict-template":
		source = strings.Replace(source, "ORIGINAL {{ issue.identifier }}", "{{ issue.nonexistent }}", 1)
	case "create-failure":
		changeHook("after_create", "printf 'created\\n' >> "+quote(hooks)+"; exit 7")
	case "create-timeout":
		changeHook("after_create", "printf 'created\\n' >> "+quote(hooks)+"; printf '%s' $$ > "+quote(filepath.Join(dir, "hook-pid"))+"; sleep 60")
	case "before-failure":
		changeHook("before_run", "printf 'before\\n' >> "+quote(hooks)+"; exit 7")
	case "before-timeout":
		changeHook("before_run", "printf 'before\\n' >> "+quote(hooks)+"; printf '%s' $$ > "+quote(filepath.Join(dir, "hook-pid"))+"; sleep 60")
	case "after-failure":
		changeHook("after_run", "printf 'after\\n' >> "+quote(hooks)+"; exit 7")
	case "after-timeout", "after-long-shutdown", "shutdown-long":
		changeHook("after_run", "printf 'after\\n' >> "+quote(hooks)+"; printf '%s' $$ > "+quote(filepath.Join(dir, "hook-pid"))+"; sleep 60")
	case "remove-failure":
		changeHook("before_remove", "printf 'removed\\n' >> "+quote(hooks)+"; exit 7")
	case "remove-timeout":
		changeHook("before_remove", "printf 'removed\\n' >> "+quote(hooks)+"; printf '%s' $$ > "+quote(filepath.Join(dir, "hook-pid"))+"; sleep 60")
	case "remove-long-shutdown":
		changeHook("before_remove", "printf 'removed\\n' >> "+quote(hooks)+"; printf '%s' $$ > "+quote(filepath.Join(dir, "hook-pid"))+"; sleep 60")
	}
	if mode == "create-timeout" {
		source = strings.Replace(source, "hooks:\n", "hooks:\n  timeout_ms: 100\n", 1)
	}
	if mode == "tool-cancel" {
		source = strings.Replace(source, "interval_ms: 40", "interval_ms: 30000", 1)
	}
	if mode == "outbound-overflow" {
		source = strings.Replace(source, "ORIGINAL {{ issue.identifier }}", strings.Repeat("x", 8192), 1)
	}
	if mode == "root-alias" {
		if err = os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "root-alias")
		if err = os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		source = strings.Replace(source, "  root: "+root, "  root: "+alias, 1)
	}
	switch mode {
	case "defaults":
		source = strings.Replace(source, "  max_turns: 1\n", "", 1)
		source = strings.Replace(source, "polling:\n  interval_ms: 40\n", "", 1)
		source = strings.Replace(source, "  active_states: [Todo]\n  terminal_states: [Done]\n", "", 1)
		source = strings.Replace(source, "project_id: PROJECT", "project_id: $RUNTIME_PROJECT_ID", 1)
	case "env-path":
		source = strings.Replace(source, "  root: "+root, "  root: $RUNTIME_WORKSPACE_ROOT", 1)
	case "tilde-path":
		source = strings.Replace(source, "  root: "+root, "  root: ~/workspaces", 1)
	case "relative-path":
		source = strings.Replace(source, "  root: "+root, "  root: workspaces", 1)
	case "null-template":
		source = strings.Replace(source, "ORIGINAL {{ issue.identifier }}", "known-null={{ issue.branch_name }}|first={{ attempt }}", 1)
	case "unknown-filter":
		source = strings.Replace(source, "ORIGINAL {{ issue.identifier }}", "{{ issue.title | unsupported_filter }}", 1)
	case "inactive-template":
		source = strings.Replace(source, "ORIGINAL {{ issue.identifier }}", "{% if false %}{{ issue.nonexistent }}{% endif %}", 1)
	case "cwd-override":
		source = strings.Replace(source, scalar(command), scalar("cd /tmp; "+command), 1)
	case "symlink", "non-directory":
		if err = os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, WorkspaceKey("o/r#3"))
		if mode == "symlink" {
			err = os.Symlink(t.TempDir(), path)
		} else {
			err = os.WriteFile(path, []byte("preserve"), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if mode == "before-timeout" || mode == "after-timeout" || mode == "remove-timeout" {
		source = strings.Replace(source, "hooks:\n", "hooks:\n  timeout_ms: 100\n", 1)
	}
	if err = os.WriteFile(workflow, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(dir, "stderr")
	f, err := os.Create(logs)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if mode == "log-sink-failure" {
		_ = f.Close()
		f, err = os.OpenFile("/dev/full", os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	cmd := exec.Command(binary) // exercises cwd default, unlike the explicit-path smoke
	if mode == "snapshot-api" {
		cmd = exec.Command(exe, "-test.run=^TestRuntimeSnapshotProcess$")
	}
	cmd.Dir = dir
	cmd.Stderr = f
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "SSL_CERT_FILE="+ca, "RUNTIME_TRACKER_SECRET=runtime-synthetic-secret", "SYMPHONY_RUNTIME_PEER="+mode, "SYMPHONY_RUNTIME_RECORD="+record, "GH_TOKEN=never-inherit", "GITHUB_TOKEN=never-inherit", "RUNTIME_WORKSPACE_ROOT="+root, "RUNTIME_PROJECT_ID=PROJECT", "HOME="+dir)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	readFile := func(path string) string { b, _ := os.ReadFile(path); return string(b) }
	waitUntil := func(check func() bool) {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			select {
			case e := <-done:
				reaped = true
				t.Fatalf("premature exit %v\n%s", e, readFile(logs))
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("scenario deadline\n%s\npeer:%s", readFile(logs), readFile(record))
	}
	unsafeWorkspace := mode == "symlink" || mode == "non-directory"
	noPeer := mode == "strict-template" || mode == "unknown-filter" || mode == "inactive-template" || mode == "cwd-override" || unsafeWorkspace || mode == "create-failure" || mode == "create-timeout" || mode == "before-failure" || mode == "before-timeout"
	waitUntil(func() bool {
		return strings.Contains(readFile(record), `"type":"prompt"`) || ((mode == "cancel-startup" || mode == "outbound-overflow" || noPeer) && (strings.Contains(readFile(hooks), "after") || len(runtimeOutcomes(readFile(logs))) > 0))
	})
	switch mode {
	case "terminal", "inactive", "missing", "ineligible", "remove-failure", "remove-timeout", "remove-long-shutdown":
		mu.Lock()
		switch mode {
		case "terminal", "remove-failure", "remove-timeout", "remove-long-shutdown":
			state = "Done"
		case "inactive":
			state = "Human Review"
		case "missing":
			visible = false
		case "ineligible":
			dispatchable = false
		}
		mu.Unlock()
	case "shutdown", "shutdown-long":
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	if !unsafeWorkspace {
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "after") })
	}
	if mode == "remove-long-shutdown" {
		waitUntil(func() bool { return strings.Contains(readFile(hooks), "removed") })
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	if mode == "after-long-shutdown" {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	if mode == "shutdown" || mode == "after-long-shutdown" || mode == "shutdown-long" || mode == "remove-long-shutdown" {
		select {
		case err = <-done:
			reaped = true
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown exceeded deadline")
		}
	} else {
		if mode != "log-sink-failure" {
			waitUntil(func() bool { return len(runtimeOutcomes(readFile(logs))) > 0 })
		}
		if mode == "snapshot-api" {
			waitUntil(func() bool { _, e := os.Stat(filepath.Join(dir, "api-snapshot")); return e == nil })
			if e := os.WriteFile(filepath.Join(dir, "api-stop"), nil, 0600); e != nil {
				t.Fatal(e)
			}
		} else {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err = <-done:
			reaped = true
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown exceeded deadline")
		}
	}
	if err != nil {
		t.Fatalf("host shutdown %v\n%s", err, readFile(logs))
	}
	raw := readFile(logs)
	if strings.Contains(raw, "runtime-synthetic-secret") {
		t.Fatal("tracker credential leaked")
	}
	outcomes := runtimeOutcomes(raw)
	if len(outcomes) == 0 && mode != "log-sink-failure" {
		t.Fatal("no structured worker outcome")
	}
	for _, event := range outcomes {
		if category == "" && event["outcome"] != "completed" {
			t.Fatalf("successful scenario failed: %v", event)
		}
		if category != "" && !(mode == "after-long-shutdown" && event["outcome"] == "canceled") && !strings.Contains(fmt.Sprint(event["error"])+" "+fmt.Sprint(event["reason"]), category) {
			t.Fatalf("wrong failure: %v want %s", event, category)
		}
	}
	path := filepath.Join(root, WorkspaceKey("o/r#3"))
	terminal := mode == "terminal" || mode == "remove-failure" || mode == "remove-timeout" || mode == "remove-long-shutdown"
	if terminal {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("terminal workspace survived")
		}
	} else {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("nonterminal workspace was destroyed", err)
		}
	}
	beforeCount := 1
	if mode == "create-failure" || mode == "create-timeout" {
		beforeCount = 0
	}
	createdCount, afterCount := 1, 1
	if unsafeWorkspace {
		createdCount, beforeCount, afterCount = 0, 0, 0
	}
	if strings.Count(readFile(hooks), "created") != createdCount || strings.Count(readFile(hooks), "before") != beforeCount || strings.Count(readFile(hooks), "after") != afterCount {
		t.Fatal("hook lifecycle wrong", readFile(hooks))
	}
	if mode == "continuation" && strings.Count(readFile(record), `"type":"prompt"`) != 2 {
		t.Fatal("continuation did not reuse process for two prompts", readFile(record))
	}
	if mode == "defaults" && strings.Count(readFile(record), `"type":"prompt"`) != 20 {
		t.Fatal("default max_turns not honored")
	}
	if mode == "null-template" && !strings.Contains(readFile(record), "known-null=|first=") {
		t.Fatal("known null or first attempt semantics wrong")
	}
	if mode == "non-directory" {
		data, _ := os.ReadFile(path)
		if string(data) != "preserve" {
			t.Fatal("non-directory workspace was destroyed")
		}
	}
	if pidBytes, e := os.ReadFile(filepath.Join(dir, "hook-pid")); e == nil {
		var pid int
		if _, e = fmt.Sscanf(string(pidBytes), "%d", &pid); e != nil {
			t.Fatal(e)
		}
		if e = syscall.Kill(pid, 0); e != syscall.ESRCH {
			t.Fatal("hook survived daemon exit", pid, e)
		}
		assertProcessGroupGone(t, pid)
	}
	if category != "" && !noPeer && mode != "cancel-startup" && mode != "after-long-shutdown" && !strings.Contains(readFile(record), `"aborted":true`) && mode != "eof" && mode != "chunk-eof" {
		t.Fatal("cancellation did not send abort", readFile(record))
	}
	if !noPeer && !bytes.Contains([]byte(readFile(record)), []byte(path)) {
		t.Fatal("agent did not launch in issue workspace")
	}
	for _, line := range strings.Split(readFile(record), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["pid"] != nil {
			if err := syscall.Kill(int(event["pid"].(float64)), 0); err != syscall.ESRCH {
				t.Fatal("agent survived daemon exit", event, err)
			}
			assertProcessGroupGone(t, int(event["pgid"].(float64)))
		}
	}
	if mode == "tool-cancel" {
		if _, e := os.Stat(filepath.Join(dir, "provider-canceled")); e != nil {
			t.Fatal("host_tool_cancel did not cancel actual provider HTTP request")
		}
	}
	t.Logf("standalone %s: actual RPC peer, hooks, host outcome, workspace lifecycle and clean SIGTERM verified", mode)
}

func runtimeOutcomes(raw string) []map[string]any {
	var outcomes []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil || event["issue_id"] != "ITEM" {
			continue
		}
		outcome := event["outcome"]
		_, hasError := event["error"]
		if (outcome == "completed" || outcome == "failed" || outcome == "canceled") && event["session_id"] != nil && event["reason"] != nil && hasError {
			outcomes = append(outcomes, event)
		}
	}
	return outcomes
}

// Public Go API consumer process, not a CLI HTTP endpoint or replaced runner.
func TestRuntimeSnapshotProcess(t *testing.T) {
	if os.Getenv("SYMPHONY_RUNTIME_PEER") != "snapshot-api" {
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	o, err := NewOrchestrator(ctx, "WORKFLOW.md", logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.Snapshot(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal("snapshot available before Run", err)
	}
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	seenRunning := false
	for ctx.Err() == nil {
		snapshot, e := o.Snapshot(ctx)
		if e == nil {
			if len(snapshot.Running) > 0 {
				seenRunning = true
			}
			if len(snapshot.Retrying) > 0 {
				if !seenRunning || snapshot.Totals.TokenUsage != (TokenUsage{11, 7, 19}) || snapshot.RateLimits != nil || snapshot.Retrying[0].Attempt != 1 {
					t.Fatalf("public API aggregate/rows wrong %+v", snapshot)
				}
				bytes, _ := json.Marshal(snapshot)
				if err = os.WriteFile("api-snapshot", bytes, 0600); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = o.Snapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("snapshot cancellation missing", err)
	}
	if err = o.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		if _, err = os.Stat("api-stop"); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = o.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("stopped snapshot available", err)
	}
}

func assertProcessGroupGone(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process group %d survived daemon exit", pgid)
}
