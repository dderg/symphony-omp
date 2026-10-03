package symphony

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type rpcTestTracker struct {
	issues  []Issue
	specs   []ToolSpec
	execute func(context.Context, string, json.RawMessage, Issue) ToolResult
}

func (t *rpcTestTracker) FetchIssuesByStates(context.Context, []string) ([]Issue, error) {
	return t.issues, nil
}
func (t *rpcTestTracker) FetchIssuesByIDs(context.Context, []string) ([]Issue, error) {
	return t.issues, nil
}
func (t *rpcTestTracker) AgentToolSpecs() []ToolSpec       { return t.specs }
func (t *rpcTestTracker) SecretEnvironmentNames() []string { return []string{"RPC_PRIVATE_TOKEN"} }
func (t *rpcTestTracker) ExecuteAgentTool(ctx context.Context, name string, args json.RawMessage, issue Issue) ToolResult {
	if t.execute != nil {
		return t.execute(ctx, name, args, issue)
	}
	return toolFailure("test provider failure")
}
func rpcCategory(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Category != want {
		t.Fatalf("error=%v, want %s", err, want)
	}
}
func rpcJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func rpcChunk(id string, index, count, length int, data []byte) []byte {
	return rpcJSON(map[string]any{"type": "rpc_chunk", "chunkId": id, "index": index, "count": count, "byteLength": length, "data": base64.StdEncoding.EncodeToString(data)})
}
func TestRPCDecoder(t *testing.T) {
	logical := []byte(`{"type":"notice","message":"世界"}`)
	d := rpcDecoder{physical: 1024, logical: 1024, v2: true}
	// Split a UTF-8 code point: validation belongs to the fully reassembled frame.
	split := bytes.Index(logical, []byte("世")) + 1
	m, e := d.decode(rpcChunk("x", 0, 2, len(logical), logical[:split]))
	if e != nil || m != nil {
		t.Fatalf("first chunk: %v %v", m, e)
	}
	m, e = d.decode(rpcChunk("x", 1, 2, len(logical), logical[split:]))
	if e != nil || rpcString(m, "message") != "世界" {
		t.Fatalf("reassembly: %v %v", m, e)
	}
	cases := map[string][][]byte{
		"not negotiated":    {rpcChunk("x", 0, 1, 2, []byte(`{}`))},
		"missing numeric":   {[]byte(`{"type":"rpc_chunk","chunkId":"x","count":1,"byteLength":2,"data":"e30="}`)},
		"null":              {[]byte(`{"type":"rpc_chunk","chunkId":"x","index":null,"count":1,"byteLength":2,"data":"e30="}`)},
		"fraction":          {[]byte(`{"type":"rpc_chunk","chunkId":"x","index":0.5,"count":1,"byteLength":2,"data":"e30="}`)},
		"wrong field type":  {[]byte(`{"type":"rpc_chunk","chunkId":3,"index":0,"count":1,"byteLength":2,"data":"e30="}`)},
		"bad base64":        {[]byte(`{"type":"rpc_chunk","chunkId":"x","index":0,"count":1,"byteLength":2,"data":"???"}`)},
		"index":             {rpcChunk("x", 1, 2, 2, []byte(`}`))},
		"different id":      {rpcChunk("x", 0, 2, 2, []byte(`{`)), rpcChunk("y", 1, 2, 2, []byte(`}`))},
		"different count":   {rpcChunk("x", 0, 2, 3, []byte(`{`)), rpcChunk("x", 1, 3, 3, []byte(`}`))},
		"duplicate":         {rpcChunk("x", 0, 2, 2, []byte(`{`)), rpcChunk("x", 0, 2, 2, []byte(`}`))},
		"interrupted":       {rpcChunk("x", 0, 2, 2, []byte(`{`)), []byte(`{"type":"notice"}`)},
		"length":            {rpcChunk("x", 0, 1, 3, []byte(`{}`))},
		"logical overflow":  {rpcChunk("x", 0, 1, 1025, []byte(`{}`))},
		"utf8":              {rpcChunk("x", 0, 1, 1, []byte{0xff})},
		"nested":            {rpcChunk("x", 0, 1, len(`{"type":"rpc_chunk"}`), []byte(`{"type":"rpc_chunk"}`))},
		"not object":        {[]byte(`[]`)},
		"physical overflow": {bytes.Repeat([]byte{' '}, 1024)},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			decoder := rpcDecoder{physical: 1024, logical: 1024, v2: name != "not negotiated"}
			var err error
			for _, frame := range frames {
				_, err = decoder.decode(frame)
				if err != nil {
					break
				}
			}
			rpcCategory(t, err, "protocol_error")
		})
	}
}

type rpcTestWriter struct{ bytes.Buffer }

func (w *rpcTestWriter) Close() error { return nil }
func rpcMemoryClient(frames ...any) *rpcClient {
	c := &rpcClient{ctx: context.Background(), stdin: &rpcTestWriter{}, frames: make(chan rpcPhysical, 64), tools: make(chan rpcToolCompletion, 16), stopped: make(chan struct{}), cfg: Config{ReadTimeout: time.Second, TurnTimeout: time.Second}, decoder: rpcDecoder{physical: rpcPhysicalLimit, logical: rpcLogicalLimit, v2: true}, pending: map[string]context.CancelFunc{}, specs: map[string]bool{}, tracker: &rpcTestTracker{}, lastOutput: time.Now()}
	for _, frame := range frames {
		c.frames <- rpcPhysical{bytes: rpcJSON(frame)}
	}
	return c
}
func rpcResponse(id, command string, data any) map[string]any {
	return map[string]any{"type": "response", "id": id, "command": command, "success": true, "data": data}
}
func TestRPCPromptCorrelationAndSettlement(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint(local), func(t *testing.T) {
			admission := rpcResponse("symphony-1", "prompt", map[string]any{"agentInvoked": !local})
			c := rpcMemoryClient(rpcResponse("unrelated", "get_state", map[string]any{}), map[string]any{"type": "turn_end"}, map[string]any{"type": "agent_end", "messages": []any{}}, admission)
			if !local {
				c.frames <- rpcPhysical{bytes: rpcJSON(map[string]any{"type": "prompt_result", "id": "another", "status": "error"})}
				c.frames <- rpcPhysical{bytes: rpcJSON(map[string]any{"type": "prompt_result", "id": "symphony-1", "status": "completed", "sessionSettled": false})}
			}
			c.frames <- rpcPhysical{bytes: rpcJSON(map[string]any{"type": "session_settled"})}
			c.frames <- rpcPhysical{bytes: rpcJSON(rpcResponse("symphony-2", "get_state", map[string]any{"isSettled": true}))}
			if e := c.prompt("hello"); e != nil {
				t.Fatal(e)
			}
			if c.turnCount != 1 || !c.settled {
				t.Fatalf("turn state: %+v", c)
			}
		})
	}
}
func TestRPCPromptFailures(t *testing.T) {
	cases := []struct {
		name, category string
		frames         []any
	}{
		{"premature", "protocol_error", []any{map[string]any{"type": "prompt_result", "id": "symphony-1", "status": "completed", "sessionSettled": true}}},
		{"agent end is not completion", "turn_timeout", []any{rpcResponse("symphony-1", "prompt", nil), map[string]any{"type": "agent_end", "messages": []any{}}}},
		{"background error", "turn_failed", []any{rpcResponse("symphony-1", "prompt", nil), map[string]any{"type": "prompt_result", "id": "symphony-1", "status": "completed", "sessionSettled": false}, map[string]any{"type": "message_end", "message": map[string]any{"stopReason": "error", "errorMessage": "background failed"}}}},
		{"abort", "turn_cancelled", []any{rpcResponse("symphony-1", "prompt", nil), map[string]any{"type": "prompt_result", "id": "symphony-1", "status": "aborted"}}},
		{"command failure", "response_error", []any{map[string]any{"type": "response", "id": "symphony-1", "command": "prompt", "success": false, "error": "no"}}},
		{"malformed success", "protocol_error", []any{map[string]any{"type": "response", "id": "symphony-1", "command": "prompt", "success": "true"}}},
		{"mismatched command", "protocol_error", []any{rpcResponse("symphony-1", "get_state", nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := rpcMemoryClient(tc.frames...)
			c.cfg.TurnTimeout = 20 * time.Millisecond
			rpcCategory(t, c.prompt("hello"), tc.category)
		})
	}
}
func TestRPCRequestRetainsInterleavedPromptResult(t *testing.T) {
	c := rpcMemoryClient(map[string]any{"type": "prompt_result", "id": "p", "status": "completed", "sessionSettled": true}, rpcResponse("symphony-1", "get_state", map[string]any{"isSettled": true}))
	c.turn = "p"
	if _, e := c.request("get_state", nil); e != nil {
		t.Fatal(e)
	}
	if len(c.deferred) != 1 || rpcString(c.deferred[0], "id") != "p" {
		t.Fatal("lost prompt result")
	}
}
func TestRPCPhysicalChunksRefreshSilence(t *testing.T) {
	c := rpcMemoryClient()
	c.cfg.TurnTimeout = 100 * time.Millisecond
	payload := []byte(`{"type":"notice"}`)
	go func() {
		for i, b := range payload {
			time.Sleep(15 * time.Millisecond)
			c.frames <- rpcPhysical{bytes: rpcChunk("x", i, len(payload), len(payload), []byte{b})}
		}
	}()
	start := time.Now()
	m, e := c.next(time.Time{}, c.cfg.TurnTimeout)
	if e != nil || rpcString(m, "type") != "notice" {
		t.Fatalf("%v %v", m, e)
	}
	if time.Since(start) <= c.cfg.TurnTimeout {
		t.Fatal("test did not exceed total cap")
	}
	c.lastOutput = time.Now().Add(-time.Second)
	rpcCategory(t, func() error { _, e := c.next(time.Time{}, 20*time.Millisecond); return e }(), "turn_timeout")
}
func TestRPCHostTools(t *testing.T) {
	c := rpcMemoryClient()
	c.specs["mutate"] = true
	c.issue = Issue{ID: "issue", NativeRef: map[string]any{"item_id": "native"}}
	started := make(chan Issue, 1)
	cancelled := make(chan struct{})
	c.tracker = &rpcTestTracker{execute: func(ctx context.Context, _ string, _ json.RawMessage, i Issue) ToolResult {
		started <- i
		<-ctx.Done()
		close(cancelled)
		return toolFailure("cancelled")
	}}
	call := map[string]any{"type": "host_tool_call", "id": "tool1", "toolName": "mutate", "arguments": map[string]any{"x": 1}}
	m, _ := rpcParse(rpcJSON(call))
	if e := c.dispatch(m); e != nil {
		t.Fatal(e)
	}
	snapshot := <-started
	if snapshot.ID != "issue" || snapshot.NativeRef["item_id"] != "native" {
		t.Fatalf("lost context: %+v", snapshot)
	}
	c.issue = Issue{ID: "different"}
	cancel, _ := rpcParse([]byte(`{"type":"host_tool_cancel","targetId":"tool1"}`))
	if e := c.dispatch(cancel); e != nil {
		t.Fatal(e)
	}
	<-cancelled
	if len(c.pending) != 0 {
		t.Fatal("cancel did not remove pending tool")
	}
	for _, request := range []string{`{"type":"host_tool_call","id":"bad","toolName":"mutate","arguments":[]}`, `{"type":"host_tool_call","id":"unsupported","toolName":"unknown","arguments":{}}`} {
		m, _ := rpcParse([]byte(request))
		if e := c.dispatch(m); e != nil {
			t.Fatal(e)
		}
	}
	lines := strings.Split(strings.TrimSpace(c.stdin.(*rpcTestWriter).String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("results: %v", lines)
	}
	for _, line := range lines {
		m, e := rpcParse([]byte(line))
		if e != nil || !rpcBool(m, "isError") {
			t.Fatalf("unstructured failure: %s", line)
		}
	}
}
func TestRPCCWDPolicy(t *testing.T) {
	for _, command := range []string{`omp --cwd=/tmp`, `omp "--cwd" /tmp`, `omp -C/tmp`, `true;cd /tmp;omp`, `true&&cd /tmp&&omp`, `bash -c 'cd /tmp; omp'`, `omp --working-directory /tmp`, `c\d /tmp;omp`} {
		if !commandCWDOverride(command) {
			t.Errorf("accepted %q", command)
		}
	}
	for _, command := range []string{`exec omp --mode rpc --no-ui`, `env NAME=value omp --mode rpc`, `/path/to/omp --config 'some file.json'`, `printf '%s' 'a cd example'`, `echo cd; exec omp`} {
		if commandCWDOverride(command) {
			t.Errorf("rejected %q", command)
		}
	}
}

// The current test executable is a deterministic protocol peer; no installed OMP
// or provider access is needed. Each helper is a separately isolated process group.
func TestRPCProcessPeer(t *testing.T) {
	mode := os.Getenv("SYMPHONY_RPC_PEER")
	if mode == "" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	write := func(v any) {
		if encoder.Encode(v) != nil {
			os.Exit(3)
		}
	}
	write(map[string]any{"type": "ready", "protocolVersion": 1, "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": rpcPhysicalLimit, "maxReassembledFrameBytes": rpcLogicalLimit})
	scanner := bufio.NewScanner(os.Stdin)
	stats := 0
	prompts := 0
	for scanner.Scan() {
		m, e := rpcParse(scanner.Bytes())
		if e != nil {
			os.Exit(4)
		}
		kind, id := rpcString(m, "type"), rpcString(m, "id")
		var data any
		switch kind {
		case "negotiate_protocol":
			data = map[string]any{"protocolVersion": 2}
		case "new_session":
			data = map[string]any{"cancelled": mode == "cancel-startup"}
		case "get_state":
			data = map[string]any{"sessionId": "native-session", "isSettled": true}
		case "set_host_tools":
		case "get_session_stats":
			stats++
			data = map[string]any{"tokens": map[string]int{"input": stats * 10, "output": stats * 2, "total": stats * 12}}
		case "prompt":
			prompts++
			if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("RPC_PRIVATE_TOKEN") != "" {
				os.Exit(5)
			}
			if prompts > 1 && strings.Contains(rpcString(m, "message"), "ORIGINAL") {
				os.Exit(6)
			}
			if mode == "silence" {
				write(rpcResponse(id, kind, nil))
				continue
			}
			write(rpcResponse(id, kind, map[string]any{"agentInvoked": true}))
			write(map[string]any{"type": "prompt_result", "id": id, "status": "completed", "sessionSettled": true})
			continue
		case "abort":
			os.Exit(0)
		default:
			os.Exit(7)
		}
		write(rpcResponse(id, kind, data))
	}
	if mode == "early-eof" {
		_ = os.Stdout.Close()
		time.Sleep(time.Minute)
	}
	if mode == "shutdown-error" {
		write(map[string]any{"type": "notice", "level": "error", "message": "persistence failed"})
	}
	os.Exit(0)
}
func rpcPeerConfig(t *testing.T, mode string) Config {
	t.Helper()
	t.Setenv("SYMPHONY_RPC_PEER", mode)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	return Config{Command: "exec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run '^TestRPCProcessPeer$'", WorkspaceRoot: t.TempDir(), ReadTimeout: 2 * time.Second, TurnTimeout: 250 * time.Millisecond, MaxTurns: 2, Hooks: Hooks{Timeout: time.Second}, Tracker: TrackerConfig{ActiveStates: []string{"In Progress"}}}
}
func TestRPCProcessStartupAndTimeout(t *testing.T) {
	for _, mode := range []string{"cancel-startup", "silence"} {
		t.Run(mode, func(t *testing.T) {
			cfg := rpcPeerConfig(t, mode)
			c, e := startRPC(context.Background(), cfg, t.TempDir(), &rpcTestTracker{}, Issue{}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer c.shutdown(true)
			e = c.startup()
			if mode == "cancel-startup" {
				rpcCategory(t, e, "startup_failed")
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			rpcCategory(t, c.prompt("test"), "turn_timeout")
		})
	}
}
func TestRPCProcessEarlyEOFShutdownBound(t *testing.T) {
	cfg := rpcPeerConfig(t, "early-eof")
	c, e := startRPC(context.Background(), cfg, t.TempDir(), &rpcTestTracker{}, Issue{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.startup(); e != nil {
		_ = c.shutdown(true)
		t.Fatal(e)
	}
	start := time.Now()
	e = c.shutdown(false)
	rpcCategory(t, e, "port_exit")
	if elapsed := time.Since(start); elapsed > rpcShutdownDeadline+2*time.Second {
		t.Fatalf("shutdown exceeded bound: %v", elapsed)
	}
	if e = syscall.Kill(-c.cmd.Process.Pid, 0); e == nil {
		t.Fatal("process group survived")
	}
}
func TestRunAttemptLifecycleAndCumulativeStats(t *testing.T) {
	cfg := rpcPeerConfig(t, "normal")
	t.Setenv("GH_TOKEN", "gh-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("RPC_PRIVATE_TOKEN", "private-secret")
	cfg.Hooks.AfterRun = "printf after > after-run"
	w := &Workflow{Prompt: "ORIGINAL {{ issue.title }}", Config: cfg}
	tracker := &rpcTestTracker{issues: []Issue{{ID: "id", Identifier: "ABC-1", Title: "work", State: "In Progress", Dispatchable: true}}}
	live := cfg
	manager := &WorkspaceManager{Config: func() Config { return live }}
	var events []AgentEvent
	e := RunAttempt(context.Background(), w, tracker, manager, tracker.issues[0], nil, func(event AgentEvent) {
		events = append(events, event)
		if event.Event == "session_started" {
			live.WorkspaceRoot = t.TempDir()
			live.Hooks.AfterRun = "printf live > after-run"
		}
	})
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(cfg.WorkspaceRoot, "ABC-1", "after-run")
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "live" {
		t.Fatalf("live hook, pinned root: %q %v", b, e)
	}
	stats := 0
	completed := 0
	for _, event := range events {
		if event.Event == "session_stats" {
			stats++
			if event.Usage.TotalTokens != int64(stats*12) {
				t.Fatalf("not cumulative: %+v", event)
			}
		}
		if event.Event == "turn_completed" {
			completed++
			if event.SessionID != "native-session-"+event.TurnID {
				t.Fatalf("bad session correlation: %+v", event)
			}
		}
	}
	if stats != 3 || completed != 2 {
		t.Fatalf("stats=%d completed=%d", stats, completed)
	}
}
func TestRunAttemptAfterRunOnHookFailures(t *testing.T) {
	for _, hook := range []string{"after_create", "before_run"} {
		t.Run(hook, func(t *testing.T) {
			cfg := rpcPeerConfig(t, "normal")
			cfg.Hooks.AfterRun = "printf cleaned > after-run"
			if hook == "after_create" {
				cfg.Hooks.AfterCreate = "exit 2"
			} else {
				cfg.Hooks.BeforeRun = "exit 2"
			}
			w := &Workflow{Config: cfg}
			manager := &WorkspaceManager{Config: func() Config { return cfg }}
			e := RunAttempt(context.Background(), w, &rpcTestTracker{}, manager, Issue{Identifier: "ABC-1"}, nil, nil)
			if e == nil {
				t.Fatal("hook failure was ignored")
			}
			b, e := os.ReadFile(filepath.Join(cfg.WorkspaceRoot, "ABC-1", "after-run"))
			if e != nil || string(b) != "cleaned" {
				t.Fatalf("after_run omitted: %q %v", b, e)
			}
		})
	}
}
func TestRPCReadOutputFramingErrors(t *testing.T) {
	for _, input := range []string{`{"type":"notice"}`, string(bytes.Repeat([]byte{'x'}, rpcPhysicalLimit)) + "\n"} {
		c := &rpcClient{stdout: io.NopCloser(strings.NewReader(input)), frames: make(chan rpcPhysical, 2), stopped: make(chan struct{})}
		go c.readOutput()
		frame := <-c.frames
		rpcCategory(t, frame.err, "protocol_error")
		<-c.stopped
	}
}
func TestRunAttemptLiveContinuationEligibility(t *testing.T) {
	for _, scenario := range []string{"closed", "locked", "archived", "label removed", "states reload", "labels reload", "terminal overlap"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := rpcPeerConfig(t, "normal")
			cfg.Tracker.RequiredLabels = []string{"route"}
			live := cfg
			initial := Issue{ID: "id", Identifier: "ABC-1", Title: "work", State: "In Progress", Dispatchable: true, Labels: []string{"route"}}
			tracker := &rpcTestTracker{issues: []Issue{initial}}
			manager := &WorkspaceManager{Config: func() Config { return live }}
			completed := 0
			e := RunAttempt(context.Background(), &Workflow{Prompt: "ORIGINAL", Config: cfg}, tracker, manager, initial, nil, func(event AgentEvent) {
				if event.Event != "turn_completed" {
					return
				}
				completed++
				switch scenario {
				case "closed", "locked", "archived":
					tracker.issues[0].Dispatchable = false
				case "label removed":
					tracker.issues[0].Labels = nil
				case "states reload":
					live.Tracker.ActiveStates = []string{"Todo"}
				case "labels reload":
					live.Tracker.RequiredLabels = []string{"different"}
				case "terminal overlap":
					live.Tracker.TerminalStates = []string{"In Progress"}
				}
			})
			if e != nil {
				t.Fatal(e)
			}
			if completed != 1 {
				t.Fatalf("ineligible issue received %d prompts", completed)
			}
		})
	}
}
func TestRPCStatsRejectInvalidTotals(t *testing.T) {
	for _, tokens := range []any{nil, map[string]any{}, map[string]any{"input": -1, "output": 2, "total": 1}, map[string]any{"input": 1, "output": nil, "total": 1}, map[string]any{"input": 1.5, "output": 2, "total": 3}} {
		c := rpcMemoryClient(rpcResponse("symphony-1", "get_session_stats", map[string]any{"tokens": tokens}))
		rpcCategory(t, c.stats(), "protocol_error")
	}
	b, e := json.Marshal(TokenUsage{1, 2, 3})
	if e != nil || string(b) != `{"input_tokens":1,"output_tokens":2,"total_tokens":3}` {
		t.Fatalf("usage wire keys: %s %v", b, e)
	}
}
func TestRPCPromptWaitsForHostToolSettlement(t *testing.T) {
	c := rpcMemoryClient(rpcResponse("symphony-1", "prompt", nil), map[string]any{"type": "host_tool_call", "id": "tool", "toolName": "read", "arguments": map[string]any{}}, map[string]any{"type": "prompt_result", "id": "symphony-1", "status": "completed", "sessionSettled": true})
	c.specs["read"] = true
	c.tracker = &rpcTestTracker{execute: func(context.Context, string, json.RawMessage, Issue) ToolResult {
		return ToolResult{Content: []ToolContent{{Type: "text", Text: "done"}}}
	}}
	if e := c.prompt("hello"); e != nil {
		t.Fatal(e)
	}
	if len(c.pending) != 0 || !strings.Contains(c.stdin.(*rpcTestWriter).String(), `"host_tool_result"`) {
		t.Fatal("turn returned before host tool result")
	}
}
func TestRPCInteractiveDialogFailsClosed(t *testing.T) {
	c := rpcMemoryClient()
	m, _ := rpcParse([]byte(`{"type":"extension_ui_request","id":"dialog","method":"confirm"}`))
	rpcCategory(t, c.dispatch(m), "turn_input_required")
	if !strings.Contains(c.stdin.(*rpcTestWriter).String(), `"cancelled":true`) {
		t.Fatal("dialog was not cancelled")
	}
}
func TestRPCNormalShutdownDrainsTrailingError(t *testing.T) {
	cfg := rpcPeerConfig(t, "shutdown-error")
	c, e := startRPC(context.Background(), cfg, t.TempDir(), &rpcTestTracker{}, Issue{}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.startup(); e != nil {
		_ = c.shutdown(true)
		t.Fatal(e)
	}
	rpcCategory(t, c.shutdown(false), "turn_failed")
}
func TestRunAttemptCancellationStillRunsAfterRun(t *testing.T) {
	cfg := rpcPeerConfig(t, "silence")
	cfg.Hooks.AfterRun = "printf cleaned > after-run"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &WorkspaceManager{Config: func() Config { return cfg }}
	e := RunAttempt(ctx, &Workflow{Config: cfg}, &rpcTestTracker{}, manager, Issue{Identifier: "ABC-1"}, nil, func(event AgentEvent) {
		if event.Event == "session_started" {
			cancel()
		}
	})
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation lost: %v", e)
	}
	b, e := os.ReadFile(filepath.Join(cfg.WorkspaceRoot, "ABC-1", "after-run"))
	if e != nil || string(b) != "cleaned" {
		t.Fatalf("after_run omitted after cancellation: %q %v", b, e)
	}
}
func TestRPCChunkEOFAndInboundLimit(t *testing.T) {
	c := rpcMemoryClient()
	c.frames <- rpcPhysical{bytes: rpcChunk("x", 0, 2, 2, []byte("{"))}
	close(c.frames)
	_, e := c.next(time.Time{}, time.Second)
	rpcCategory(t, e, "protocol_error")
	c = rpcMemoryClient()
	c.decoder.physical = 32
	rpcCategory(t, c.send(map[string]any{"type": "prompt", "message": strings.Repeat("x", 32)}), "protocol_error")
}
func TestRPCReadinessLimitsAndNegotiation(t *testing.T) {
	c := rpcMemoryClient(map[string]any{"type": "ready", "supportedProtocolVersions": []int{1, 2}, "maxFrameBytes": 4096, "maxReassembledFrameBytes": 8192},
		rpcResponse("symphony-1", "negotiate_protocol", map[string]any{"protocolVersion": 2}),
		rpcResponse("symphony-2", "new_session", map[string]any{"cancelled": false}),
		rpcResponse("symphony-3", "get_state", map[string]any{"sessionId": "native"}),
		rpcResponse("symphony-4", "set_host_tools", nil))
	c.decoder.v2 = false
	if e := c.startup(); e != nil {
		t.Fatal(e)
	}
	if !c.decoder.v2 || c.decoder.physical != 4096 || c.decoder.logical != 8192 || c.native != "native" {
		t.Fatal("ready limits or session identity ignored")
	}
	for _, limit := range []any{0, -1, nil, "1024", 1.5} {
		c := rpcMemoryClient(map[string]any{"type": "ready", "maxFrameBytes": limit})
		rpcCategory(t, c.startup(), "protocol_error")
	}
}
