package symphony

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// Target: oh-my-pi 18.4.12, d19afc28611dd6fa485160659eb42a4da36022e9.
const rpcPhysicalLimit = 1048576
const rpcLogicalLimit = 67108864
const rpcShutdownDeadline = 5 * time.Second

type rpcObject map[string]json.RawMessage

func rpcString(m rpcObject, k string) string { var s string; _ = json.Unmarshal(m[k], &s); return s }
func rpcBool(m rpcObject, k string) bool     { var b bool; _ = json.Unmarshal(m[k], &b); return b }
func rpcData(m rpcObject) rpcObject          { var d rpcObject; _ = json.Unmarshal(m["data"], &d); return d }
func rpcParse(b []byte) (rpcObject, error) {
	if !utf8.Valid(b) {
		return nil, fault("protocol_error", "invalid UTF-8")
	}
	var m rpcObject
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, fault("protocol_error", "frame must be a JSON object")
	}
	return m, nil
}

type rpcDecoder struct {
	physical, logical   int
	v2                  bool
	id                  string
	count, length, next int
	buffer              []byte
}

func (d *rpcDecoder) decode(b []byte) (rpcObject, error) {
	if len(b)+1 > d.physical {
		return nil, fault("protocol_error", "physical frame overflow")
	}
	m, err := rpcParse(b)
	if err != nil {
		return nil, err
	}
	if rpcString(m, "type") != "rpc_chunk" {
		if d.id != "" {
			return nil, fault("protocol_error", "interrupted chunk sequence")
		}
		return m, nil
	}
	if !d.v2 {
		return nil, fault("protocol_error", "chunk received before v2 negotiation")
	}
	var c struct {
		ChunkID    string `json:"chunkId"`
		Index      int    `json:"index"`
		Count      int    `json:"count"`
		ByteLength int    `json:"byteLength"`
		Data       string `json:"data"`
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return nil, fault("protocol_error", "invalid chunk fields")
	}
	// Missing numeric fields must not silently become valid zero values.
	for _, key := range []string{"chunkId", "index", "count", "byteLength", "data"} {
		if len(m[key]) == 0 || string(m[key]) == "null" {
			return nil, fault("protocol_error", "missing chunk field")
		}
	}
	if c.ChunkID == "" || c.Count < 1 || c.Count > c.ByteLength || c.ByteLength < 1 || c.ByteLength > d.logical || c.Index < 0 || c.Index >= c.Count {
		return nil, fault("protocol_error", "invalid chunk bounds")
	}
	if d.id == "" {
		if c.Index != 0 {
			return nil, fault("protocol_error", "chunk sequence must start at zero")
		}
		d.id = c.ChunkID
		d.count = c.Count
		d.length = c.ByteLength
		d.buffer = make([]byte, 0, c.ByteLength)
	}
	if c.ChunkID != d.id || c.Count != d.count || c.ByteLength != d.length || c.Index != d.next {
		return nil, fault("protocol_error", "inconsistent chunk sequence")
	}
	part, e := base64.StdEncoding.Strict().DecodeString(c.Data)
	if e != nil || len(part) == 0 || len(d.buffer)+len(part) > d.length {
		return nil, fault("protocol_error", "invalid chunk data")
	}
	d.buffer = append(d.buffer, part...)
	d.next++
	if d.next != d.count {
		return nil, nil
	}
	if len(d.buffer) != d.length {
		return nil, fault("protocol_error", "chunk byte length mismatch")
	}
	result, e := rpcParse(d.buffer)
	d.id = ""
	d.next = 0
	d.buffer = nil
	if e == nil && rpcString(result, "type") == "rpc_chunk" {
		return nil, fault("protocol_error", "nested chunk frame")
	}
	return result, e
}

type rpcPhysical struct {
	bytes []byte
	err   error
}
type rpcToolCompletion struct {
	id     string
	result ToolResult
}
type rpcClient struct {
	ctx               context.Context
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	stdout            io.ReadCloser
	frames            chan rpcPhysical
	exited            chan error
	stopped           chan struct{}
	tools             chan rpcToolCompletion
	decoder           rpcDecoder
	cfg               Config
	tracker           Tracker
	issue             Issue
	specs             map[string]bool
	pending           map[string]context.CancelFunc
	sequence          int
	writeMu           sync.Mutex
	emit              func(AgentEvent)
	native, turn, pid string
	turnCount         int
	settled           bool
	deferred          []rpcObject
	lastOutput        time.Time
}

func sanitizedEnvironment(names []string) []string { return childEnvironment(names) }

// Tokenize only for policy inspection; bash receives the original command unchanged.
func commandCWDOverride(command string) bool {
	var token strings.Builder
	var quote rune
	escaped := false
	words := []string{}
	flush := func() {
		if token.Len() > 0 {
			words = append(words, token.String())
			token.Reset()
		}
	}
	for _, r := range command {
		if escaped {
			token.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				token.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if strings.ContainsRune(" \t\r\n;&|()<>`", r) {
			flush()
			if strings.ContainsRune("\n;&|()`", r) {
				words = append(words, ";")
			}
			continue
		}
		token.WriteRune(r)
	}
	flush()
	commandStart := true
	for i, word := range words {
		if word == ";" {
			commandStart = true
			continue
		}
		if word == "--cwd" || strings.HasPrefix(word, "--cwd=") || word == "--working-directory" || strings.HasPrefix(word, "--working-directory=") || strings.HasPrefix(word, "-C") {
			return true
		}
		if commandStart && (word == "cd" || word == "pushd" || word == "popd") {
			return true
		}
		if (word == "-c" || word == "-lc") && i+1 < len(words) && commandCWDOverride(words[i+1]) {
			return true
		}
		if commandStart && (word == "exec" || word == "command" || word == "builtin" || word == "env" || strings.Contains(word, "=")) {
			continue
		}
		commandStart = false
	}
	return false
}
func startRPC(ctx context.Context, cfg Config, path string, tracker Tracker, issue Issue, emit func(AgentEvent)) (*rpcClient, error) {
	if commandCWDOverride(cfg.Command) {
		return nil, fault("invalid_workspace_cwd", "command redirects workspace cwd")
	}
	cmd := exec.Command("bash", "-lc", cfg.Command)
	cmd.Dir = path
	cmd.Env = sanitizedEnvironment(tracker.SecretEnvironmentNames())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, fault("omp_not_found", err.Error())
	}
	c := &rpcClient{ctx: ctx, cmd: cmd, stdin: stdin, stdout: stdout, frames: make(chan rpcPhysical, 16), exited: make(chan error, 1), stopped: make(chan struct{}), tools: make(chan rpcToolCompletion, 16), decoder: rpcDecoder{physical: rpcPhysicalLimit, logical: rpcLogicalLimit}, cfg: cfg, tracker: tracker, issue: issue, pending: map[string]context.CancelFunc{}, specs: map[string]bool{}, emit: emit, pid: strconv.Itoa(cmd.Process.Pid)}
	c.lastOutput = time.Now()
	go func() { _, _ = io.Copy(io.Discard, stderr); stderr.Close() }()
	go c.readOutput()
	// Wait only after stdout has been drained: Wait closes pipe descriptors.
	go func() { <-c.stopped; c.exited <- cmd.Wait() }()
	return c, nil
}
func (c *rpcClient) readOutput() {
	defer close(c.frames)
	defer close(c.stopped)
	r := bufio.NewReaderSize(c.stdout, 64*1024)
	for {
		var line []byte
		for {
			part, e := r.ReadSlice('\n')
			if len(line)+len(part) > rpcPhysicalLimit {
				c.frames <- rpcPhysical{err: fault("protocol_error", "physical frame overflow")}
				return
			}
			line = append(line, part...)
			if e == bufio.ErrBufferFull {
				continue
			}
			if e != nil {
				if e == io.EOF && len(line) > 0 {
					e = fault("protocol_error", "unterminated physical frame")
				}
				if e != io.EOF {
					c.frames <- rpcPhysical{err: e}
				}
				return
			}
			break
		}
		c.frames <- rpcPhysical{bytes: line[:len(line)-1]}
	}
}
func (c *rpcClient) event(name, message string, usage *TokenUsage) {
	if c.emit != nil {
		session := ""
		if c.native != "" && c.turn != "" {
			session = c.native + "-" + c.turn
		}
		c.emit(AgentEvent{Event: name, Timestamp: time.Now().UTC(), PID: c.pid, SessionID: session, NativeSessionID: c.native, TurnID: c.turn, Message: message, Usage: usage, TurnCount: c.turnCount})
	}
}
func (c *rpcClient) send(m any) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	if len(b)+1 > c.decoder.physical {
		return fault("protocol_error", "inbound command exceeds physical frame limit")
	}
	b = append(b, '\n')
	done := make(chan error, 1)
	go func() { c.writeMu.Lock(); defer c.writeMu.Unlock(); _, e := c.stdin.Write(b); done <- e }()
	timer := time.NewTimer(c.cfg.ReadTimeout)
	defer timer.Stop()
	select {
	case e := <-done:
		return e
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-timer.C:
		return fault("response_timeout", "stdin write timeout")
	}
}
func (c *rpcClient) id() string { c.sequence++; return fmt.Sprintf("symphony-%d", c.sequence) }
func toolFailure(message string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: "text", Text: message}}, IsError: true}
}
func (c *rpcClient) toolResult(id string, r ToolResult) error {
	return c.send(map[string]any{"type": "host_tool_result", "id": id, "result": r, "isError": r.IsError})
}
func (c *rpcClient) dispatch(m rpcObject) error {
	switch rpcString(m, "type") {
	case "rpc_frame_error":
		return fault("protocol_error", "RPC transport overflow")
	case "response":
		var success bool
		if json.Unmarshal(m["success"], &success) != nil || string(m["success"]) == "null" {
			return fault("protocol_error", "response missing boolean success")
		}
		// A delayed failure for this turn remains relevant during synchronous stats
		// or settlement requests; failures for unrelated IDs cannot fail this turn.
		if !success && c.turn != "" && rpcString(m, "id") == c.turn {
			return fault("response_error", rpcString(m, "error"))
		}
	case "host_tool_call":
		id, name := rpcString(m, "id"), rpcString(m, "toolName")
		if id == "" {
			return fault("protocol_error", "host tool request missing id")
		}
		if _, ok := c.pending[id]; ok {
			return fault("protocol_error", "duplicate host tool request")
		}
		args := m["arguments"]
		object, e := rpcParse(args)
		if e != nil || object == nil {
			return c.toolResult(id, toolFailure("arguments must be a JSON object"))
		}
		if !c.specs[name] {
			c.event("unsupported_tool_call", name, nil)
			return c.toolResult(id, toolFailure("unsupported tool: "+name))
		}
		ctx, cancel := context.WithCancel(c.ctx)
		c.pending[id] = cancel
		issue := c.issue
		go func() {
			r := c.tracker.ExecuteAgentTool(ctx, name, args, issue)
			if ctx.Err() != nil {
				r = toolFailure("host tool cancelled; a submitted mutation may already have taken effect")
			}
			select {
			case c.tools <- rpcToolCompletion{id, r}:
			case <-c.stopped:
			}
		}()
	case "host_tool_cancel":
		id := rpcString(m, "targetId")
		if cancel, ok := c.pending[id]; ok {
			cancel()
			delete(c.pending, id)
			return c.toolResult(id, toolFailure("host tool cancelled; a submitted mutation may already have taken effect"))
		}
	case "host_uri_request":
		return c.send(map[string]any{"type": "host_uri_result", "id": rpcString(m, "id"), "isError": true, "error": "unsupported host URI"})
	case "extension_ui_request":
		switch rpcString(m, "method") {
		case "select", "confirm", "input", "editor", "ask":
			if e := c.send(map[string]any{"type": "extension_ui_response", "id": rpcString(m, "id"), "cancelled": true}); e != nil {
				return e
			}
			c.event("turn_input_required", "interactive dialog rejected", nil)
			return fault("turn_input_required", "interactive dialog rejected")
		default:
			c.event("notification", rpcString(m, "method"), nil)
		}
	case "session_settled":
		c.settled = true
	case "agent_start":
		c.settled = false
	case "message_end":
		var msg rpcObject
		_ = json.Unmarshal(m["message"], &msg)
		if rpcString(msg, "stopReason") == "error" {
			c.event("turn_ended_with_error", rpcString(msg, "errorMessage"), nil)
			return fault("turn_failed", rpcString(msg, "errorMessage"))
		}
	case "agent_end":
		var messages []rpcObject
		_ = json.Unmarshal(m["messages"], &messages)
		for _, msg := range messages {
			if rpcString(msg, "stopReason") == "error" {
				return fault("turn_failed", rpcString(msg, "errorMessage"))
			}
		}
	case "notice":
		if rpcString(m, "level") == "error" {
			return fault("turn_failed", rpcString(m, "message"))
		}
	}
	return nil
}

// Every physical output frame refreshes silence, including partial chunk sequences.
func (c *rpcClient) next(deadline time.Time, silence time.Duration) (rpcObject, error) {
	remaining := silence
	if !c.lastOutput.IsZero() {
		remaining = time.Until(c.lastOutput.Add(silence))
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for {
		var admission <-chan time.Time
		var t *time.Timer
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return nil, fault("response_timeout", "command response timeout")
			}
			t = time.NewTimer(left)
			admission = t.C
		}
		select {
		case <-c.ctx.Done():
			if t != nil {
				t.Stop()
			}
			return nil, c.ctx.Err()
		case <-admission:
			return nil, fault("response_timeout", "command response timeout")
		case <-timer.C:
			if t != nil {
				t.Stop()
			}
			return nil, fault("turn_timeout", "RPC output silence timeout")
		case result := <-c.tools:
			if t != nil {
				t.Stop()
			}
			if cancel, ok := c.pending[result.id]; ok {
				cancel()
				delete(c.pending, result.id)
				if e := c.toolResult(result.id, result.result); e != nil {
					return nil, e
				}
				return rpcObject{"type": json.RawMessage(`"host_tool_finished"`)}, nil
			}
		case frame, ok := <-c.frames:
			if t != nil {
				t.Stop()
			}
			if !ok {
				if c.decoder.id != "" {
					return nil, fault("protocol_error", "EOF during chunk sequence")
				}
				return nil, fault("port_exit", "RPC stdout closed unexpectedly")
			}
			if frame.err != nil {
				return nil, frame.err
			}
			c.lastOutput = time.Now()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(silence)
			c.event("other_message", "RPC output", nil)
			m, e := c.decoder.decode(frame.bytes)
			if e != nil {
				c.event("malformed", e.Error(), nil)
				return nil, e
			}
			if m == nil {
				continue
			}
			if e = c.dispatch(m); e != nil {
				return nil, e
			}
			return m, nil
		}
	}
}
func (c *rpcClient) request(kind string, fields map[string]any) (rpcObject, error) {
	id := c.id()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"] = kind
	fields["id"] = id
	if e := c.send(fields); e != nil {
		return nil, e
	}
	silence := c.cfg.ReadTimeout
	if c.turn != "" && !c.settled {
		if c.cfg.TurnTimeout < silence {
			silence = c.cfg.TurnTimeout
		}
	} else {
		c.lastOutput = time.Now()
	}
	deadline := time.Now().Add(c.cfg.ReadTimeout)
	for {
		m, e := c.next(deadline, silence)
		if e != nil {
			return nil, e
		}
		if rpcString(m, "type") == "response" && rpcString(m, "id") == id {
			if rpcString(m, "command") != kind {
				return nil, fault("protocol_error", "response command mismatch")
			}
			if !rpcBool(m, "success") {
				return nil, fault("response_error", rpcString(m, "error"))
			}
			return rpcData(m), nil
		}
		if rpcString(m, "type") == "prompt_result" && rpcString(m, "id") == c.turn {
			c.deferred = append(c.deferred, m)
		}
	}
}
func (c *rpcClient) startup() error {
	deadline := time.Now().Add(c.cfg.ReadTimeout)
	var ready rpcObject
	for {
		m, e := c.next(deadline, c.cfg.ReadTimeout)
		if e != nil {
			return e
		}
		if rpcString(m, "type") == "ready" {
			ready = m
			break
		}
	}
	for key, dst := range map[string]*int{"maxFrameBytes": &c.decoder.physical, "maxReassembledFrameBytes": &c.decoder.logical} {
		if b, ok := ready[key]; ok {
			var n int
			if json.Unmarshal(b, &n) != nil || n < 1 {
				return fault("protocol_error", "invalid ready transport limits")
			}
			if n < *dst {
				*dst = n
			}
		}
	}
	var versions []int
	_ = json.Unmarshal(ready["supportedProtocolVersions"], &versions)
	for _, v := range versions {
		if v == 2 {
			data, e := c.request("negotiate_protocol", map[string]any{"protocolVersion": 2})
			if e != nil {
				return e
			}
			var negotiated int
			_ = json.Unmarshal(data["protocolVersion"], &negotiated)
			if negotiated != 2 {
				return fault("protocol_error", "v2 negotiation mismatch")
			}
			c.decoder.v2 = true
			break
		}
	}
	data, e := c.request("new_session", nil)
	if e != nil {
		return e
	}
	if rpcBool(data, "cancelled") {
		return fault("startup_failed", "fresh session transition cancelled")
	}
	data, e = c.request("get_state", nil)
	if e != nil {
		return e
	}
	c.native = rpcString(data, "sessionId")
	if c.native == "" {
		return fault("protocol_error", "missing native session ID")
	}
	specs := c.tracker.AgentToolSpecs()
	if specs == nil {
		specs = []ToolSpec{}
	}
	for _, spec := range specs {
		if spec.Name == "" || c.specs[spec.Name] {
			return fault("protocol_error", "invalid host tool definitions")
		}
		c.specs[spec.Name] = true
	}
	if _, e = c.request("set_host_tools", map[string]any{"tools": specs}); e != nil {
		return e
	}
	c.event("session_started", "fresh session", nil)
	return nil
}
func (c *rpcClient) prompt(message string) error {
	c.turn = c.id()
	c.turnCount++
	c.settled = false
	if e := c.send(map[string]any{"type": "prompt", "id": c.turn, "message": message}); e != nil {
		return e
	}
	c.lastOutput = time.Now()
	deadline := time.Now().Add(c.cfg.ReadTimeout)
	admitted, completed := false, false
	for {
		var m rpcObject
		var e error
		if len(c.deferred) > 0 {
			m = c.deferred[0]
			c.deferred = c.deferred[1:]
		} else {
			m, e = c.next(deadline, c.cfg.TurnTimeout)
		}
		if e != nil {
			return e
		}
		kind := rpcString(m, "type")
		if kind == "response" && rpcString(m, "id") == c.turn {
			if rpcString(m, "command") != "prompt" {
				return fault("protocol_error", "prompt response command mismatch")
			}
			admitted = true
			deadline = time.Time{}
			data := rpcData(m)
			if value, ok := data["agentInvoked"]; ok && string(value) == "false" {
				completed = true
				state, e := c.request("get_state", nil)
				if e != nil {
					return e
				}
				c.settled = c.settled || rpcBool(state, "isSettled")
			}
		}
		if kind == "prompt_result" && rpcString(m, "id") == c.turn {
			if !admitted {
				return fault("protocol_error", "result before prompt admission")
			}
			switch rpcString(m, "status") {
			case "error":
				return fault("turn_failed", string(m["error"]))
			case "aborted":
				return fault("turn_cancelled", "prompt aborted")
			case "completed":
				completed = true
				c.settled = rpcBool(m, "sessionSettled") || c.settled
			default:
				return fault("protocol_error", "invalid prompt result status")
			}
			if !c.settled {
				state, e := c.request("get_state", nil)
				if e != nil {
					return e
				}
				c.settled = c.settled || rpcBool(state, "isSettled")
			}
		}
		if completed && c.settled && len(c.pending) == 0 {
			return nil
		}
	}
}
func (c *rpcClient) stats() error {
	data, e := c.request("get_session_stats", nil)
	if e != nil {
		return e
	}
	tokens, e := rpcParse(data["tokens"])
	if e != nil {
		return fault("protocol_error", "invalid session token totals")
	}
	var usage TokenUsage
	for key, dst := range map[string]*int64{"input": &usage.InputTokens, "output": &usage.OutputTokens, "total": &usage.TotalTokens} {
		raw, ok := tokens[key]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, dst) != nil || *dst < 0 {
			return fault("protocol_error", "invalid session token total "+key)
		}
	}
	c.event("session_stats", "", &usage)
	return nil
}
func (c *rpcClient) shutdown(abort bool) error {
	for id, cancel := range c.pending {
		cancel()
		delete(c.pending, id)
	}
	// Keep the reader's channel even after EOF disables the select arm.
	drain := c.frames
	frames := c.frames
	deadline := time.NewTimer(rpcShutdownDeadline)
	defer deadline.Stop()
	defer func() {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		_ = c.stdin.Close()
		_ = c.stdout.Close()
	}()
	if abort {
		go func() {
			c.writeMu.Lock()
			defer c.writeMu.Unlock()
			_, _ = io.WriteString(c.stdin, "{\"type\":\"abort\"}\n")
			_ = c.stdin.Close()
		}()
	} else {
		_ = c.stdin.Close()
	}
	var failure error
	consume := func(frame rpcPhysical) {
		if frame.err != nil {
			failure = frame.err
			return
		}
		m, e := c.decoder.decode(frame.bytes)
		if e != nil {
			failure = e
		} else if m != nil {
			if e = c.dispatch(m); e != nil {
				failure = e
			}
		}
	}
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			consume(frame)
		case e := <-c.exited:
			// Wait is published only after the reader has closed drain; trailing notices
			// still matter even when process exit wins the select against buffered output.
			for frame := range drain {
				consume(frame)
			}
			if e != nil && !abort {
				return fault("port_exit", e.Error())
			}
			if !abort && c.decoder.id != "" {
				return fault("protocol_error", "EOF during chunk sequence")
			}
			return failure
		case <-deadline.C:
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			_ = c.stdin.Close()
			_ = c.stdout.Close()
			// A separate bound covers both a blocked output reader and process Wait.
			reap := time.NewTimer(time.Second)
			defer reap.Stop()
			for drain != nil {
				select {
				case _, ok := <-drain:
					if !ok {
						drain = nil
					}
				case <-reap.C:
					if abort {
						return nil
					}
					return fault("port_exit", "shutdown reader did not terminate")
				}
			}
			select {
			case <-c.exited:
			case <-reap.C:
				if abort {
					return nil
				}
				return fault("port_exit", "shutdown process did not terminate")
			}
			if abort {
				return nil
			}
			return fault("port_exit", "normal shutdown exceeded five seconds")
		}
	}
}
