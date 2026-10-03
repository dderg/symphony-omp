package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type schedulerTracker struct {
	issues              []Issue
	candidates          []Issue
	err                 error
	stateCalls, idCalls int
}

func (f *schedulerTracker) FetchIssuesByStates(context.Context, []string) ([]Issue, error) {
	f.stateCalls++
	return append([]Issue(nil), f.candidates...), f.err
}
func (f *schedulerTracker) FetchIssuesByIDs(_ context.Context, ids []string) ([]Issue, error) {
	f.idCalls++
	if f.err != nil {
		return nil, f.err
	}
	var out []Issue
	for _, id := range ids {
		for _, i := range f.issues {
			if i.ID == id {
				out = append(out, i)
			}
		}
	}
	return out, nil
}
func (*schedulerTracker) AgentToolSpecs() []ToolSpec       { return nil }
func (*schedulerTracker) SecretEnvironmentNames() []string { return nil }
func (*schedulerTracker) ExecuteAgentTool(context.Context, string, json.RawMessage, Issue) ToolResult {
	return ToolResult{}
}
func schedulerIssue(id, state string) Issue {
	return Issue{ID: id, Identifier: id, Title: "work", State: state, Dispatchable: true}
}
func schedulerFixture(t *testing.T) (*Orchestrator, *schedulerTracker) {
	t.Helper()
	cfg := Config{Tracker: TrackerConfig{Kind: "github_projects", ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}}, WorkspaceRoot: t.TempDir(), Hooks: Hooks{Timeout: time.Second}, PollInterval: time.Hour, MaxConcurrent: 2, MaxBackoff: time.Minute, ByState: map[string]int{}}
	w := &Workflow{Path: "fixture", Source: []byte("good"), Config: cfg}
	f := &schedulerTracker{}
	o := newOrchestrator(w, f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Unix(1000, 0)
	o.now = func() time.Time { return now }
	o.load = func(string) (*Workflow, error) { return w, nil }
	o.adapter = func(context.Context, TrackerConfig, *slog.Logger) (Tracker, error) { return f, nil }
	o.runner = func(context.Context, *Workflow, Tracker, *WorkspaceManager, Issue, *int, func(AgentEvent)) error {
		return nil
	}
	return o, f
}
func schedulerMessage(t *testing.T, o *Orchestrator) workerMessage {
	t.Helper()
	select {
	case m := <-o.messages:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("worker message deadline")
		return workerMessage{}
	}
}
func schedulerFinish(t *testing.T, o *Orchestrator) {
	t.Helper()
	for {
		m := schedulerMessage(t, o)
		if m.event != nil {
			o.agentEvent(*m.event)
		}
		if m.outcome != nil {
			o.finished(context.Background(), *m.outcome, false)
			return
		}
	}
}

func TestSchedulerEligibilitySortingAndCapacity(t *testing.T) {
	o, f := schedulerFixture(t)
	i := schedulerIssue("one", "Todo")
	if !o.eligible(i, false) {
		t.Fatal("valid issue rejected")
	}
	o.workflow.Config.Tracker.RequiredLabels = []string{"Ready"}
	i.Labels = []string{" ready "}
	if !o.eligible(i, false) {
		t.Fatal("normalized label rejected")
	}
	for _, mutate := range []func(*Issue){func(i *Issue) { i.ID = "" }, func(i *Issue) { i.Identifier = "" }, func(i *Issue) { i.Title = "" }, func(i *Issue) { i.State = "Done" }, func(i *Issue) { i.Dispatchable = false }, func(i *Issue) { i.Labels = nil }} {
		bad := i
		mutate(&bad)
		if o.eligible(bad, false) {
			t.Fatalf("invalid issue accepted: %+v", bad)
		}
	}
	o.claimed[i.ID] = true
	if o.eligible(i, false) || !o.eligible(i, true) {
		t.Fatal("claim exclusion incorrect")
	}
	delete(o.claimed, i.ID)
	p1, p2, p5 := 1, 2, 5
	old, newer := time.Unix(1, 0), time.Unix(2, 0)
	a, b, c, d := schedulerIssue("a", "Todo"), schedulerIssue("b", "Todo"), schedulerIssue("c", "Todo"), schedulerIssue("d", "Todo")
	a.Priority = &p2
	b.Priority = &p1
	c.Priority = &p1
	d.Priority = &p5
	b.CreatedAt = &newer
	c.CreatedAt = &old
	issues := []Issue{d, a, b, c}
	sortIssues(issues)
	var ids []string
	for _, i := range issues {
		ids = append(ids, i.ID)
	}
	if !reflect.DeepEqual(ids, []string{"c", "b", "a", "d"}) {
		t.Fatal(ids)
	}
	o.workflow.Config.Tracker.RequiredLabels = nil
	o.workflow.Config.ByState = map[string]int{"todo": 1}
	f.candidates = issues
	f.issues = issues
	o.tick(context.Background())
	if len(o.running) != 1 || o.running["c"] == nil {
		t.Fatal("priority/per-state dispatch failed")
	}
	schedulerFinish(t, o)
	o.workflow.Config.ByState = nil
	o.retries = map[string]retryEntry{}
	o.claimed = map[string]bool{}
	o.tick(context.Background())
	if len(o.running) != 2 || o.running["b"] == nil || o.running["c"] == nil {
		t.Fatal("global capacity failed")
	}
	schedulerFinish(t, o)
	schedulerFinish(t, o)
}

func TestSchedulerRetryClaimsAndBackoff(t *testing.T) {
	o, f := schedulerFixture(t)
	i := schedulerIssue("retry", "Todo")
	f.issues = []Issue{i}
	o.launch(context.Background(), i, 0)
	schedulerFinish(t, o)
	r := o.retries[i.ID]
	if r.attempt != 1 || r.due.Sub(o.now()) != time.Second || !o.claimed[i.ID] || !o.completed[i.ID] {
		t.Fatal("continuation retry incorrect", r)
	}
	now := r.due
	o.now = func() time.Time { return now }
	o.retryDue(context.Background())
	if o.running[i.ID] == nil || len(o.retries) != 0 {
		t.Fatal("own claim prevented retry")
	}
	schedulerFinish(t, o)
	o.runner = func(context.Context, *Workflow, Tracker, *WorkspaceManager, Issue, *int, func(AgentEvent)) error {
		return errors.New("failed")
	}
	delete(o.claimed, i.ID)
	delete(o.retries, i.ID)
	o.launch(context.Background(), i, 2)
	schedulerFinish(t, o)
	r = o.retries[i.ID]
	if r.attempt != 3 || r.due.Sub(o.now()) != 40*time.Second {
		t.Fatal("failure retry incorrect", r)
	}
	if retryDelay(100, time.Minute) != time.Minute {
		t.Fatal("backoff cap incorrect")
	}
	now = r.due
	o.valid = false
	o.retryDue(context.Background())
	if !o.claimed[i.ID] || o.running[i.ID] != nil || len(o.retries) != 1 {
		t.Fatal("invalid config dispatched or lost claim")
	}
	o.valid = true
	o.schedule(i, 1, "", true)
	now = o.retries[i.ID].due
	o.workflow.Config.MaxConcurrent = 0
	o.retryDue(context.Background())
	if o.retries[i.ID].reason != "no available orchestrator slots" || !o.claimed[i.ID] {
		t.Fatal("slot retry incorrect")
	}
	f.issues = nil
	now = o.retries[i.ID].due
	o.retryDue(context.Background())
	if o.claimed[i.ID] || len(o.retries) != 0 {
		t.Fatal("omitted retry claim retained")
	}
	for _, state := range []string{"Done", "Review"} {
		i.State = state
		f.issues = []Issue{i}
		o.schedule(i, 1, "", true)
		now = o.retries[i.ID].due
		o.retryDue(context.Background())
		if o.claimed[i.ID] || len(o.retries) != 0 {
			t.Fatal("ineligible retry claim retained", state)
		}
	}
}

func TestSchedulerReconciliationCancellation(t *testing.T) {
	for _, scenario := range []string{"missing", "terminal", "inactive", "unroutable", "stalled", "refresh_error"} {
		t.Run(scenario, func(t *testing.T) {
			o, f := schedulerFixture(t)
			i := schedulerIssue("running", "Todo")
			f.issues = []Issue{i}
			o.workflow.Config.StallTimeout = time.Second
			release := make(chan struct{})
			observed := make(chan struct{})
			o.runner = func(ctx context.Context, _ *Workflow, _ Tracker, _ *WorkspaceManager, _ Issue, _ *int, _ func(AgentEvent)) error {
				close(observed)
				<-release
				return ctx.Err()
			}
			o.launch(context.Background(), i, 0)
			<-observed
			switch scenario {
			case "missing":
				f.issues = nil
			case "terminal":
				f.issues[0].State = "Done"
			case "inactive":
				f.issues[0].State = "Review"
			case "unroutable":
				f.issues[0].Dispatchable = false
			case "stalled":
				o.now = func() time.Time { return time.Unix(1002, 0) }
			case "refresh_error":
				f.err = errors.New("offline")
			}
			o.reconcile(context.Background())
			r := o.running[i.ID]
			if scenario == "refresh_error" {
				if r.reason != "" {
					t.Fatal("fetch error canceled worker")
				}
			} else if r.reason == "" {
				t.Fatal("worker not canceled")
			}
			if len(o.running) != 1 {
				t.Fatal("worker removed before it stopped")
			}
			close(release)
			schedulerFinish(t, o)
			if scenario == "stalled" || scenario == "refresh_error" {
				if !o.claimed[i.ID] || len(o.retries) != 1 {
					t.Fatal("retry missing")
				}
			} else if o.claimed[i.ID] || len(o.retries) != 0 {
				t.Fatal("canceled worker retained claim")
			}
		})
	}
}

func TestSchedulerFIFOFinalUsageAndNativeDedup(t *testing.T) {
	o, _ := schedulerFixture(t)
	i := schedulerIssue("usage", "Todo")
	o.runner = func(_ context.Context, _ *Workflow, _ Tracker, _ *WorkspaceManager, _ Issue, _ *int, emit func(AgentEvent)) error {
		for _, u := range []TokenUsage{{10, 20, 35}, {10, 20, 35}, {12, 25, 41}} {
			usage := u
			emit(AgentEvent{NativeSessionID: "native", SessionID: "native-turn", Usage: &usage})
		}
		return nil
	}
	o.launch(context.Background(), i, 0)
	schedulerFinish(t, o)
	if o.totals != (TokenUsage{12, 25, 41}) {
		t.Fatal("final usage lost or double counted", o.totals)
	}
	delete(o.claimed, i.ID)
	delete(o.retries, i.ID)
	o.launch(context.Background(), i, 0)
	schedulerFinish(t, o)
	if o.totals != (TokenUsage{12, 25, 41}) {
		t.Fatal("native session totals counted twice", o.totals)
	}
}

func TestSchedulerContentWatcherAndInvalidPreflight(t *testing.T) {
	o, f := schedulerFixture(t)
	calls := 0
	o.adapter = func(context.Context, TrackerConfig, *slog.Logger) (Tracker, error) { calls++; return f, nil }
	for range 4 {
		o.reload(context.Background(), false)
	}
	if calls != 0 {
		t.Fatal("unchanged watcher constructed adapter", calls)
	}
	o.reload(context.Background(), true)
	if calls != 1 {
		t.Fatal("preflight omitted")
	}
	prior := o.workflow
	next := *prior
	next.Source = []byte("changed")
	next.Config.MaxConcurrent = 3
	o.load = func(string) (*Workflow, error) { return &next, nil }
	o.reload(context.Background(), false)
	if calls != 2 || o.workflow != &next || o.live.MaxConcurrent != 3 {
		t.Fatal("changed workflow not applied")
	}
	i := schedulerIssue("active", "Todo")
	o.launch(context.Background(), i, 0)
	f.issues = nil
	o.adapter = func(context.Context, TrackerConfig, *slog.Logger) (Tracker, error) {
		return nil, errors.New("invalid scope")
	}
	o.reload(context.Background(), true)
	if o.valid || o.workflow != &next {
		t.Fatal("known-good config lost")
	}
	o.launch(context.Background(), schedulerIssue("blocked", "Todo"), 0)
	if o.running["blocked"] != nil {
		t.Fatal("invalid config allowed launch")
	}
	o.tick(context.Background())
	if o.running[i.ID].reason != "missing" {
		t.Fatal("invalid dispatch blocked reconciliation")
	}
	schedulerFinish(t, o)
}

func TestSchedulerRootReloadCleanupAfterWorkerStops(t *testing.T) {
	o, f := schedulerFixture(t)
	i := schedulerIssue("root", "Todo")
	original := o.workflow.Config.WorkspaceRoot
	replacement := t.TempDir()
	created := make(chan *WorkspaceManager, 1)
	release := make(chan struct{})
	o.runner = func(ctx context.Context, _ *Workflow, _ Tracker, m *WorkspaceManager, i Issue, _ *int, _ func(AgentEvent)) error {
		if _, err := m.Create(ctx, i.Identifier); err != nil {
			return err
		}
		created <- m
		<-release
		return ctx.Err()
	}
	o.launch(context.Background(), i, 0)
	var manager *WorkspaceManager
	select {
	case manager = <-created:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace not created")
	}
	cfg := o.currentConfig()
	cfg.WorkspaceRoot = replacement
	cfg.Hooks.AfterRun = "printf live"
	o.configMu.Lock()
	o.live = cfg
	o.configMu.Unlock()
	if manager.Config().WorkspaceRoot != original || manager.Config().Hooks.AfterRun != "printf live" {
		t.Fatal("root not pinned or hooks not live")
	}
	replacementPath := filepath.Join(replacement, WorkspaceKey(i.Identifier))
	if err := os.Mkdir(replacementPath, 0700); err != nil {
		t.Fatal(err)
	}
	f.issues = []Issue{i}
	f.issues[0].State = "Done"
	o.reconcile(context.Background())
	originalPath := filepath.Join(original, WorkspaceKey(i.Identifier))
	if _, err := os.Stat(originalPath); err != nil {
		t.Fatal("cleanup occurred before runner stopped", err)
	}
	close(release)
	schedulerFinish(t, o)
	if _, err := os.Stat(originalPath); !os.IsNotExist(err) {
		t.Fatal("original workspace not removed", err)
	}
	if _, err := os.Stat(replacementPath); err != nil {
		t.Fatal("replacement workspace removed", err)
	}
}

func TestSchedulerSnapshotUnavailableTimeoutAndRows(t *testing.T) {
	o, _ := schedulerFixture(t)
	if _, err := o.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := o.Refresh(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	o.started = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := o.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := o.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := o.Snapshot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	i := schedulerIssue("snapshot", "Todo")
	o.running[i.ID] = &runningAttempt{issue: i, started: o.now().Add(-2 * time.Second), turnCount: 3}
	o.schedule(schedulerIssue("queued", "Todo"), 2, "failure", false)
	s := o.snapshot()
	if len(s.Running) != 1 || s.Running[0].TurnCount != 3 || len(s.Retrying) != 1 || s.Totals.SecondsRunning != 2 || s.RateLimits != nil {
		t.Fatal(s)
	}
	close(o.done)
	if _, err := o.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestSchedulerRunSnapshotRefreshAndCleanShutdown(t *testing.T) {
	o, f := schedulerFixture(t)
	i := schedulerIssue("lifecycle", "Todo")
	f.candidates = []Issue{i}
	f.issues = []Issue{i}
	launched := make(chan struct{})
	stopped := make(chan struct{})
	o.runner = func(ctx context.Context, _ *Workflow, _ Tracker, _ *WorkspaceManager, _ Issue, _ *int, emit func(AgentEvent)) error {
		close(launched)
		<-ctx.Done()
		usage := TokenUsage{2, 3, 7}
		emit(AgentEvent{NativeSessionID: "final", Usage: &usage})
		close(stopped)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- o.Run(ctx) }()
	select {
	case <-launched:
	case <-time.After(2 * time.Second):
		t.Fatal("initial dispatch did not run")
	}
	s, err := o.Snapshot(context.Background())
	if err != nil || len(s.Running) != 1 {
		t.Fatal("live snapshot failed", s, err)
	}
	if err := o.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("shutdown returned before worker")
	}
	if o.totals != (TokenUsage{2, 3, 7}) || len(o.running) != 0 || len(o.claimed) != 0 {
		t.Fatal("shutdown lost final state", o.totals)
	}
	if _, err := o.Snapshot(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err == nil {
		t.Fatal("second Run accepted")
	}
}

func TestSchedulerReloadAffectsLiveStallAndFutureAdapter(t *testing.T) {
	o, old := schedulerFixture(t)
	i := schedulerIssue("active", "Todo")
	old.issues = []Issue{i}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	o.running[i.ID] = &runningAttempt{issue: i, started: o.now().Add(-time.Minute), cancel: cancel}
	o.workflow.Config.StallTimeout = 0
	o.reconcile(context.Background())
	if ctx.Err() != nil {
		t.Fatal("live stall disable ignored")
	}
	o.workflow.Config.StallTimeout = 2 * time.Minute
	o.reconcile(context.Background())
	if ctx.Err() != nil {
		t.Fatal("live increased stall interval ignored")
	}
	next := schedulerIssue("newly-visible", "Todo")
	next.Title = "Work exposed by refreshed adapter"
	fresh := &schedulerTracker{issues: []Issue{i, next}, candidates: []Issue{next}}
	o.adapter = func(context.Context, TrackerConfig, *slog.Logger) (Tracker, error) { return fresh, nil }
	seen := make(chan Issue, 1)
	o.runner = func(_ context.Context, _ *Workflow, _ Tracker, _ *WorkspaceManager, issue Issue, _ *int, _ func(AgentEvent)) error {
		seen <- issue
		return nil
	}
	// Unchanged workflow preflight must make newly visible work dispatchable.
	o.tick(context.Background())
	select {
	case dispatched := <-seen:
		if dispatched.ID != next.ID || dispatched.Title != next.Title {
			t.Fatalf("dispatched wrong work: %+v", dispatched)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("new adapter's eligible issue was not dispatched")
	}
	if active := o.running[i.ID]; active == nil || active.issue.State != "Todo" || ctx.Err() != nil {
		t.Fatal("original active issue was no longer managed")
	}
	schedulerFinish(t, o)
	o.workflow.Config.StallTimeout = time.Second
	o.reconcile(context.Background())
	if ctx.Err() == nil {
		t.Fatal("live decreased stall timeout ignored")
	}
}

func TestSchedulerTerminalRetryCleansAttemptRootAfterReload(t *testing.T) {
	o, tracker := schedulerFixture(t)
	issue := schedulerIssue("old-root", "Todo")
	original := o.workflow.Config.WorkspaceRoot
	path, err := o.workspaces.Create(context.Background(), issue.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	o.launch(context.Background(), issue, 0)
	schedulerFinish(t, o)
	replacement := t.TempDir()
	o.workflow.Config.WorkspaceRoot = replacement
	o.configMu.Lock()
	o.live.WorkspaceRoot = replacement
	o.configMu.Unlock()
	if _, err = os.Stat(filepath.Join(original, issue.Identifier)); err != nil {
		t.Fatal(err)
	}
	issue.State = "Done"
	tracker.issues = []Issue{issue}
	// Capture the due time: retryDue removes the entry before subsequent now calls.
	due := o.retries[issue.ID].due
	o.now = func() time.Time { return due }
	o.retryDue(context.Background())
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("terminal retry left the previous attempt workspace behind", err)
	}
}

func TestSchedulerTerminalAfterRootRetryCleansEveryKnownWorkspace(t *testing.T) {
	o, tracker := schedulerFixture(t)
	issue := schedulerIssue("root-retry", "Todo")
	old, err := o.workspaces.Create(context.Background(), issue.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	o.launch(context.Background(), issue, 0)
	schedulerFinish(t, o)
	replacement := t.TempDir()
	o.workflow.Config.WorkspaceRoot = replacement
	o.configMu.Lock()
	o.live.WorkspaceRoot = replacement
	o.configMu.Unlock()
	fresh, err := o.workspaces.Create(context.Background(), issue.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	tracker.issues = []Issue{issue}
	due := o.retries[issue.ID].due
	o.now = func() time.Time { return due }
	o.retryDue(context.Background())
	issue.State = "Done"
	tracker.issues = []Issue{issue}
	o.reconcile(context.Background())
	schedulerFinish(t, o)
	for _, path := range []string{old, fresh} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("terminal transition left known attempt workspace", path, err)
		}
	}
}
