package symphony

import (
	"context"
	"errors"
	"sort"
	"time"
)

type SessionSnapshot struct {
	IssueID            string     `json:"issue_id"`
	Identifier         string     `json:"issue_identifier"`
	URL                *string    `json:"issue_url"`
	State              string     `json:"state"`
	SessionID          string     `json:"session_id"`
	NativeSessionID    string     `json:"native_session_id"`
	PID                string     `json:"pid"`
	TurnID             string     `json:"turn_id"`
	TurnCount          int        `json:"turn_count"`
	StartedAt          time.Time  `json:"started_at"`
	LastEventAt        *time.Time `json:"last_event_at"`
	LastEvent          string     `json:"last_event"`
	LastMessage        string     `json:"last_message"`
	Tokens             TokenUsage `json:"tokens"`
	CancellationReason *string    `json:"cancellation_reason"`
}
type RetrySnapshot struct {
	IssueID    string    `json:"issue_id"`
	Identifier string    `json:"issue_identifier"`
	URL        *string   `json:"issue_url"`
	Attempt    int       `json:"attempt"`
	DueAt      time.Time `json:"due_at"`
	Error      *string   `json:"error"`
}
type RuntimeTotals struct {
	TokenUsage
	SecondsRunning float64 `json:"seconds_running"`
}
type Snapshot struct {
	GeneratedAt   time.Time         `json:"generated_at"`
	Running       []SessionSnapshot `json:"running"`
	Retrying      []RetrySnapshot   `json:"retrying"`
	Totals        RuntimeTotals     `json:"omp_totals"`
	RateLimits    any               `json:"rate_limits"`
	DispatchValid bool              `json:"dispatch_valid"`
}
type snapshotRequest struct{ reply chan Snapshot }

var ErrUnavailable = errors.New("orchestrator unavailable")

// Snapshot and Refresh impose a five-second ceiling even without a caller deadline.
func (o *Orchestrator) Snapshot(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	o.startMu.Lock()
	started := o.started
	o.startMu.Unlock()
	if !started {
		return Snapshot{}, ErrUnavailable
	}
	req := snapshotRequest{make(chan Snapshot, 1)}
	select {
	case <-o.done:
		return Snapshot{}, ErrUnavailable
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case o.requests <- req:
	}
	select {
	case <-o.done:
		return Snapshot{}, ErrUnavailable
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case result := <-req.reply:
		return result, nil
	}
}
func (o *Orchestrator) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	o.startMu.Lock()
	started := o.started
	o.startMu.Unlock()
	if !started {
		return ErrUnavailable
	}
	reply := make(chan error, 1)
	select {
	case <-o.done:
		return ErrUnavailable
	case <-ctx.Done():
		return ctx.Err()
	case o.refresh <- reply:
	}
	select {
	case <-o.done:
		return ErrUnavailable
	case <-ctx.Done():
		return ctx.Err()
	case err := <-reply:
		return err
	}
}
func (o *Orchestrator) snapshot() Snapshot {
	now := o.now()
	s := Snapshot{GeneratedAt: now, Running: []SessionSnapshot{}, Retrying: []RetrySnapshot{}, Totals: RuntimeTotals{o.totals, o.seconds}, RateLimits: nil, DispatchValid: o.valid}
	for _, r := range o.running {
		var last *time.Time
		if !r.timeLast.IsZero() {
			v := r.timeLast
			last = &v
		}
		var reason *string
		if r.reason != "" {
			v := r.reason
			reason = &v
		}
		s.Running = append(s.Running, SessionSnapshot{r.issue.ID, r.issue.Identifier, r.issue.URL, r.issue.State, r.sessionID, r.nativeID, r.pid, r.turnID, r.turnCount, r.started, last, r.lastEvent, r.message, r.tokens, reason})
		s.Totals.SecondsRunning += now.Sub(r.started).Seconds()
	}
	for id, r := range o.retries {
		var reason *string
		if r.reason != "" {
			v := r.reason
			reason = &v
		}
		s.Retrying = append(s.Retrying, RetrySnapshot{id, r.issue.Identifier, r.issue.URL, r.attempt, r.due, reason})
	}
	sort.Slice(s.Running, func(i, j int) bool { return s.Running[i].Identifier < s.Running[j].Identifier })
	sort.Slice(s.Retrying, func(i, j int) bool {
		if s.Retrying[i].DueAt.Equal(s.Retrying[j].DueAt) {
			return s.Retrying[i].Identifier < s.Retrying[j].Identifier
		}
		return s.Retrying[i].DueAt.Before(s.Retrying[j].DueAt)
	})
	return s
}
