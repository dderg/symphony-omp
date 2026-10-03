package symphony

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Orchestrator owns all scheduling state inside Run's select loop.
// A workflow and adapter snapshot is retained by each worker.
type Orchestrator struct {
	path            string
	logger          *slog.Logger
	workflow        *Workflow
	tracker         Tracker
	workspaces      *WorkspaceManager
	configMu        sync.RWMutex
	live            Config
	load            func(string) (*Workflow, error)
	adapter         func(context.Context, TrackerConfig, *slog.Logger) (Tracker, error)
	runner          func(context.Context, *Workflow, Tracker, *WorkspaceManager, Issue, *int, func(AgentEvent)) error
	now             func() time.Time
	running         map[string]*runningAttempt
	knownWorkspaces map[string]map[string]*WorkspaceManager
	retries         map[string]retryEntry
	claimed         map[string]bool
	completed       map[string]bool
	usage           map[string]TokenUsage
	totals          TokenUsage
	seconds         float64
	valid           bool
	messages        chan workerMessage
	requests        chan snapshotRequest
	refresh         chan chan error
	done            chan struct{}
	startMu         sync.Mutex
	started         bool
}
type runningAttempt struct {
	issue                                                Issue
	attempt                                              int
	started, timeLast                                    time.Time
	cancel                                               context.CancelCauseFunc
	reason                                               string
	cleanup                                              bool
	manager                                              *WorkspaceManager
	sessionID, nativeID, pid, turnID, lastEvent, message string
	turnCount                                            int
	tokens                                               TokenUsage
}
type retryEntry struct {
	issue   Issue
	attempt int
	due     time.Time
	reason  string
}
type workerOutcome struct {
	id    string
	err   error
	cause error
}
type workerEvent struct {
	id    string
	event AgentEvent
}

// Events and completion share one FIFO so final usage is applied before removal.
type workerMessage struct {
	event   *workerEvent
	outcome *workerOutcome
}

func NewOrchestrator(ctx context.Context, path string, logger *slog.Logger) (*Orchestrator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	w, err := LoadWorkflow(path)
	if err != nil {
		return nil, err
	}
	tracker, err := NewTracker(ctx, w.Config.Tracker, logger)
	if err != nil {
		return nil, err
	}
	o := newOrchestrator(w, tracker, logger)
	o.workspaces = &WorkspaceManager{Config: o.currentConfig, Logger: logger, Secrets: tracker.SecretEnvironmentNames()}
	return o, nil
}
func newOrchestrator(w *Workflow, t Tracker, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = slog.Default()
	}
	o := &Orchestrator{path: w.Path, logger: logger, workflow: w, tracker: t, live: w.Config, load: LoadWorkflow, adapter: NewTracker, runner: RunAttempt, now: time.Now, running: map[string]*runningAttempt{}, retries: map[string]retryEntry{}, claimed: map[string]bool{}, completed: map[string]bool{}, usage: map[string]TokenUsage{}, valid: true, messages: make(chan workerMessage, 256), requests: make(chan snapshotRequest), refresh: make(chan chan error), done: make(chan struct{})}
	o.knownWorkspaces = map[string]map[string]*WorkspaceManager{}
	o.workspaces = &WorkspaceManager{Config: o.currentConfig, Logger: logger, Secrets: t.SecretEnvironmentNames()}
	return o
}
func (o *Orchestrator) currentConfig() Config {
	o.configMu.RLock()
	defer o.configMu.RUnlock()
	return o.live
}
func (o *Orchestrator) logIssue(level slog.Level, i Issue, msg string, args ...any) {
	attrs := []any{"issue_id", i.ID, "issue_identifier", i.Identifier}
	hasSession := false
	for n := 0; n+1 < len(args); n += 2 {
		if args[n] == "session_id" {
			hasSession = true
			break
		}
	}
	if !hasSession {
		attrs = append(attrs, "session_id", "")
	}
	attrs = append(attrs, args...)
	o.logger.Log(context.Background(), level, msg, attrs...)
}
func (o *Orchestrator) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 15*time.Second)
}

// Run returns successfully on requested shutdown after workers have stopped.
// The shutdown bound is an abnormal error if a runner violates cancellation.
func (o *Orchestrator) Run(ctx context.Context) error {
	o.startMu.Lock()
	if o.started {
		o.startMu.Unlock()
		return errors.New("orchestrator already started")
	}
	o.started = true
	o.startMu.Unlock()
	defer close(o.done)
	canceled := ctx.Done()
	o.startupCleanup(ctx)
	poll := time.NewTimer(0)
	defer poll.Stop()
	reload := time.NewTicker(time.Second)
	defer reload.Stop()
	retry := time.NewTimer(time.Hour)
	if !retry.Stop() {
		<-retry.C
	}
	defer retry.Stop()
	var shutdown <-chan time.Time
	var shutdownTimer *time.Timer
	defer func() {
		if shutdownTimer != nil {
			shutdownTimer.Stop()
		}
	}()
	stopping := false
	for {
		if stopping && len(o.running) == 0 {
			return nil
		}
		if !retry.Stop() {
			select {
			case <-retry.C:
			default:
			}
		}
		var retryC <-chan time.Time
		if !stopping {
			if due, ok := o.nextRetry(); ok {
				delay := due.Sub(o.now())
				if delay < 0 {
					delay = 0
				}
				retry.Reset(delay)
				retryC = retry.C
			}
		}
		select {
		case <-canceled:
			if !stopping {
				stopping = true
				for _, r := range o.running {
					o.stop(r, "shutdown", false)
				}
				o.retries = map[string]retryEntry{}
				shutdownTimer = time.NewTimer(15 * time.Second)
				shutdown = shutdownTimer.C
			}
			// Disable the permanently ready cancellation arm while draining workers.
			canceled = nil
		case <-shutdown:
			return errors.New("worker shutdown deadline exceeded")
		case <-poll.C:
			if !stopping {
				o.tick(ctx)
				poll.Reset(o.workflow.Config.PollInterval)
			}
		case <-reload.C:
			if !stopping {
				old := o.workflow.Config.PollInterval
				o.reload(ctx, false)
				if old != o.workflow.Config.PollInterval {
					if !poll.Stop() {
						select {
						case <-poll.C:
						default:
						}
					}
					poll.Reset(o.workflow.Config.PollInterval)
				}
			}
		case <-retryC:
			if !stopping {
				o.reload(ctx, true)
				o.retryDue(ctx)
			}
		case message := <-o.messages:
			if message.event != nil {
				o.agentEvent(*message.event)
			} else if message.outcome != nil {
				o.finished(ctx, *message.outcome, stopping)
			}
		case req := <-o.requests:
			req.reply <- o.snapshot()
		case reply := <-o.refresh:
			if stopping {
				reply <- errors.New("orchestrator shutting down")
			} else {
				if !poll.Stop() {
					select {
					case <-poll.C:
					default:
					}
				}
				poll.Reset(0)
				reply <- nil
			}
		}
	}
}
func (o *Orchestrator) startupCleanup(ctx context.Context) {
	c, cancel := o.operation(ctx)
	defer cancel()
	issues, err := o.tracker.FetchIssuesByStates(c, o.workflow.Config.Tracker.TerminalStates)
	if err != nil {
		o.logger.Warn("startup cleanup fetch failed", "error", err)
		return
	}
	for _, i := range issues {
		o.cleanup(ctx, i)
	}
}
func (o *Orchestrator) cleanup(ctx context.Context, i Issue) { o.cleanupWith(ctx, i, o.workspaces) }
func (o *Orchestrator) cleanupWith(ctx context.Context, i Issue, manager *WorkspaceManager) {
	managers := o.knownWorkspaces[i.ID]
	if managers == nil {
		managers = map[string]*WorkspaceManager{}
	}
	managers[manager.Config().WorkspaceRoot] = manager
	for _, manager := range managers {
		bound := *manager
		bound.Logger = o.logger.With("issue_id", i.ID, "issue_identifier", i.Identifier, "session_id", "")
		c, cancel := context.WithTimeout(ctx, bound.Config().Hooks.Timeout+time.Second)
		err := bound.Remove(c, i.Identifier)
		cancel()
		if err != nil {
			o.logIssue(slog.LevelWarn, i, "workspace cleanup failed", "outcome", "failed", "error", err)
		}
	}
	delete(o.knownWorkspaces, i.ID)
}
func (o *Orchestrator) reload(ctx context.Context, forceValidation bool) {
	w, err := o.load(o.path)
	if err == nil {
		changed := !bytes.Equal(w.Source, o.workflow.Source)
		if !changed && !forceValidation {
			return
		}
		// Watcher checks never make provider requests for unchanged content.
		// Dispatch and retry cycles still preflight environment and provider scope.
		c, cancel := o.operation(ctx)
		t, e := o.adapter(c, w.Config.Tracker, o.logger)
		cancel()
		err = e
		if err == nil {
			if changed {
				o.workflow = w
				o.configMu.Lock()
				o.live = w.Config
				o.configMu.Unlock()
				o.logger.Info("workflow reloaded", "path", w.Path)
			}
			o.tracker = t
			o.valid = true
			return
		}
	}
	o.valid = false
	o.logger.Error("workflow dispatch validation failed", "error", err)
}
func (o *Orchestrator) tick(ctx context.Context) {
	o.reconcile(ctx)
	o.reload(ctx, true)
	if !o.valid {
		return
	}
	c, cancel := o.operation(ctx)
	issues, err := o.tracker.FetchIssuesByStates(c, o.workflow.Config.Tracker.ActiveStates)
	cancel()
	if err != nil {
		o.logger.Warn("candidate fetch failed", "error", err)
		return
	}
	sortIssues(issues)
	for _, i := range issues {
		if !o.eligible(i, false) || !o.slots(i) {
			continue
		}
		// A candidate page can be stale by the time a slot opens. Refresh opaque ID.
		c, cancel := o.operation(ctx)
		fresh, err := o.tracker.FetchIssuesByIDs(c, []string{i.ID})
		cancel()
		if err != nil {
			o.logIssue(slog.LevelWarn, i, "candidate revalidation failed", "error", err)
			continue
		}
		latest, ok := findIssue(fresh, i.ID)
		if !ok {
			continue
		}
		if containsState(o.workflow.Config.Tracker.TerminalStates, latest.State) {
			o.cleanup(ctx, latest)
			continue
		}
		if o.eligible(latest, false) && o.slots(latest) {
			o.launch(ctx, latest, 0)
		}
	}
}
func sortIssues(issues []Issue) {
	sort.SliceStable(issues, func(a, b int) bool {
		x, y := issues[a], issues[b]
		rank := func(i Issue) int {
			if i.Priority != nil && *i.Priority >= 1 && *i.Priority <= 4 {
				return *i.Priority
			}
			return 5
		}
		if rank(x) != rank(y) {
			return rank(x) < rank(y)
		}
		if x.CreatedAt == nil && y.CreatedAt != nil {
			return false
		}
		if x.CreatedAt != nil && y.CreatedAt == nil {
			return true
		}
		if x.CreatedAt != nil && !x.CreatedAt.Equal(*y.CreatedAt) {
			return x.CreatedAt.Before(*y.CreatedAt)
		}
		return x.Identifier < y.Identifier
	})
}
func (o *Orchestrator) eligible(i Issue, ownClaim bool) bool {
	c := o.workflow.Config
	return strings.TrimSpace(i.ID) != "" && strings.TrimSpace(i.Identifier) != "" && strings.TrimSpace(i.Title) != "" && strings.TrimSpace(i.State) != "" && containsState(c.Tracker.ActiveStates, i.State) && !containsState(c.Tracker.TerminalStates, i.State) && routable(c, i) && o.running[i.ID] == nil && (!o.claimed[i.ID] || ownClaim)
}
func (o *Orchestrator) slots(i Issue) bool {
	c := o.workflow.Config
	if len(o.running) >= c.MaxConcurrent {
		return false
	}
	limit, ok := c.ByState[normalize(i.State)]
	if !ok {
		limit = c.MaxConcurrent
	}
	n := 0
	for _, r := range o.running {
		if normalize(r.issue.State) == normalize(i.State) {
			n++
		}
	}
	return n < limit
}
func findIssue(issues []Issue, id string) (Issue, bool) {
	for _, i := range issues {
		if i.ID == id {
			return i, true
		}
	}
	return Issue{}, false
}
func (o *Orchestrator) launch(ctx context.Context, i Issue, attempt int) {
	if !o.valid || o.running[i.ID] != nil || (!o.eligible(i, attempt > 0)) || !o.slots(i) {
		return
	}
	workerCtx, cancel := context.WithCancelCause(ctx)
	r := &runningAttempt{issue: i, attempt: attempt, started: o.now(), cancel: cancel}
	o.running[i.ID] = r
	o.claimed[i.ID] = true
	delete(o.retries, i.ID)
	w, t := o.workflow, o.tracker
	// Scripts and hook deadlines stay live; the root belongs to this attempt.
	manager := *o.workspaces
	manager.Secrets = t.SecretEnvironmentNames()
	manager.Logger = o.logger.With("issue_id", i.ID, "issue_identifier", i.Identifier, "session_id", "")
	root := w.Config.WorkspaceRoot
	manager.Config = func() Config { c := o.currentConfig(); c.WorkspaceRoot = root; return c }
	r.manager = &manager
	if o.knownWorkspaces[i.ID] == nil {
		o.knownWorkspaces[i.ID] = map[string]*WorkspaceManager{}
	}
	o.knownWorkspaces[i.ID][root] = &manager
	o.logIssue(slog.LevelInfo, i, "worker launched", "outcome", "starting", "attempt", attempt, "session_id", "")
	go func() {
		var a *int
		if attempt > 0 {
			n := attempt
			a = &n
		}
		err := o.runner(workerCtx, w, t, &manager, i, a, func(e AgentEvent) {
			event := workerEvent{i.ID, e}
			select {
			case o.messages <- workerMessage{event: &event}:
			case <-o.done:
			}
		})
		result := workerOutcome{i.ID, err, context.Cause(workerCtx)}
		select {
		case o.messages <- workerMessage{outcome: &result}:
		case <-o.done:
		}
	}()
}
func (o *Orchestrator) stop(r *runningAttempt, reason string, cleanup bool) {
	if r.reason == "" || reason == "terminal" || reason == "shutdown" {
		r.reason = reason
	}
	r.cleanup = r.cleanup || cleanup
	r.cancel(errors.New(r.reason))
	o.logIssue(slog.LevelInfo, r.issue, "worker cancellation requested", "outcome", "canceling", "reason", r.reason, "session_id", r.sessionID)
}
func (o *Orchestrator) reconcile(ctx context.Context) {
	now := o.now()
	ids := make([]string, 0, len(o.running))
	for id, r := range o.running {
		ids = append(ids, id)
		last := r.timeLast
		if last.IsZero() {
			last = r.started
		}
		if timeout := o.workflow.Config.StallTimeout; timeout > 0 && now.Sub(last) > timeout && r.reason == "" {
			o.stop(r, "stalled", false)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	c, cancel := o.operation(ctx)
	issues, err := o.tracker.FetchIssuesByIDs(c, ids)
	cancel()
	if err != nil {
		o.logger.Warn("reconciliation fetch failed", "error", err)
		return
	}
	for _, id := range ids {
		r := o.running[id]
		i, ok := findIssue(issues, id)
		if !ok {
			o.stop(r, "missing", false)
			continue
		}
		if containsState(o.workflow.Config.Tracker.TerminalStates, i.State) {
			r.issue = i
			o.stop(r, "terminal", true)
		} else if !containsState(o.workflow.Config.Tracker.ActiveStates, i.State) || !routable(o.workflow.Config, i) {
			r.issue = i
			o.stop(r, "ineligible", false)
		} else {
			r.issue = i
		}
	}
}
func retryDelay(attempt int, max time.Duration) time.Duration {
	delay := 10 * time.Second
	if max <= delay {
		return max
	}
	for n := 1; n < attempt; n++ {
		if delay >= max/2 {
			return max
		}
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
}
func (o *Orchestrator) schedule(i Issue, attempt int, reason string, continuation bool) {
	delay := retryDelay(attempt, o.workflow.Config.MaxBackoff)
	if continuation {
		delay = time.Second
	}
	o.claimed[i.ID] = true
	o.retries[i.ID] = retryEntry{issue: i, attempt: attempt, due: o.now().Add(delay), reason: reason}
	o.logIssue(slog.LevelInfo, i, "worker retry queued", "outcome", "retrying", "attempt", attempt, "delay_ms", delay.Milliseconds(), "reason", reason)
}
func (o *Orchestrator) nextRetry() (time.Time, bool) {
	var due time.Time
	for _, r := range o.retries {
		if due.IsZero() || r.due.Before(due) {
			due = r.due
		}
	}
	return due, !due.IsZero()
}
func (o *Orchestrator) retryDue(ctx context.Context) {
	ids := []string{}
	for id, r := range o.retries {
		if !r.due.After(o.now()) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := o.retries[id]
		if !o.valid {
			o.schedule(r.issue, r.attempt, "dispatch validation failed", false)
			continue
		}
		c, cancel := o.operation(ctx)
		issues, err := o.tracker.FetchIssuesByIDs(c, []string{id})
		cancel()
		if err != nil {
			o.schedule(r.issue, r.attempt+1, "retry refresh failed", false)
			continue
		}
		i, ok := findIssue(issues, id)
		if !ok {
			delete(o.claimed, id)
			delete(o.retries, id)
			continue
		}
		if containsState(o.workflow.Config.Tracker.TerminalStates, i.State) {
			o.cleanup(ctx, i)
			delete(o.claimed, id)
			delete(o.retries, id)
			continue
		}
		if !o.eligible(i, true) {
			delete(o.claimed, id)
			delete(o.retries, id)
			continue
		}
		if !o.slots(i) {
			o.schedule(i, r.attempt+1, "no available orchestrator slots", false)
			continue
		}
		o.launch(ctx, i, r.attempt)
	}
}
func (o *Orchestrator) finished(ctx context.Context, result workerOutcome, stopping bool) {
	r := o.running[result.id]
	if r == nil {
		return
	}
	delete(o.running, result.id)
	o.seconds += o.now().Sub(r.started).Seconds()
	if r.cleanup {
		o.cleanupWith(ctx, r.issue, r.manager)
	}
	reason := r.reason
	if reason == "" && result.cause != nil {
		reason = result.cause.Error()
	}
	outcome := "completed"
	if result.err != nil {
		outcome = "failed"
	}
	if reason != "" {
		outcome = "canceled"
	}
	o.logIssue(slog.LevelInfo, r.issue, "worker exited", "outcome", outcome, "session_id", r.sessionID, "reason", reason, "error", result.err)
	if stopping || (reason != "" && reason != "stalled") {
		delete(o.claimed, result.id)
		return
	}
	if result.err == nil && reason == "" {
		o.completed[result.id] = true
		o.schedule(r.issue, 1, "", true)
	} else {
		message := reason
		if message == "" {
			message = fmt.Sprint(result.err)
		}
		o.schedule(r.issue, r.attempt+1, message, false)
	}
}
func positiveDelta(next, last int64) int64 {
	if next > last {
		return next - last
	}
	return 0
}
func (o *Orchestrator) agentEvent(update workerEvent) {
	r := o.running[update.id]
	if r == nil {
		return
	}
	e := update.event
	r.timeLast = o.now()
	if !e.Timestamp.IsZero() && e.Timestamp.After(r.timeLast) {
		r.timeLast = e.Timestamp
	}
	r.lastEvent = e.Event
	r.message = e.Message
	if e.PID != "" {
		r.pid = e.PID
	}
	if e.SessionID != "" {
		r.sessionID = e.SessionID
	}
	if e.NativeSessionID != "" {
		r.nativeID = e.NativeSessionID
	}
	if e.TurnID != "" {
		r.turnID = e.TurnID
	}
	if e.TurnCount > r.turnCount {
		r.turnCount = e.TurnCount
	}
	if e.Usage != nil && r.nativeID != "" {
		last := o.usage[r.nativeID]
		u := *e.Usage
		delta := TokenUsage{positiveDelta(u.InputTokens, last.InputTokens), positiveDelta(u.OutputTokens, last.OutputTokens), positiveDelta(u.TotalTokens, last.TotalTokens)}
		o.totals.InputTokens += delta.InputTokens
		o.totals.OutputTokens += delta.OutputTokens
		o.totals.TotalTokens += delta.TotalTokens
		r.tokens.InputTokens += delta.InputTokens
		r.tokens.OutputTokens += delta.OutputTokens
		r.tokens.TotalTokens += delta.TotalTokens
		if u.InputTokens > last.InputTokens {
			last.InputTokens = u.InputTokens
		}
		if u.OutputTokens > last.OutputTokens {
			last.OutputTokens = u.OutputTokens
		}
		if u.TotalTokens > last.TotalTokens {
			last.TotalTokens = u.TotalTokens
		}
		o.usage[r.nativeID] = last
	}
	o.logIssue(slog.LevelInfo, r.issue, "agent event", "outcome", "received", "session_id", r.sessionID, "native_session_id", r.nativeID, "event", e.Event, "turn_count", r.turnCount)
}
