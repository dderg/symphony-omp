package symphony

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Actual shell hook cancellation must follow host cancellation during cleanup.
func TestTerminalCleanupHonorsHostCancellation(t *testing.T) {
	o, _ := schedulerFixture(t)
	cfg := o.currentConfig()
	cfg.Hooks.BeforeRemove = "printf '%s' $$ > hook-started; sleep 30"
	cfg.Hooks.Timeout = time.Minute
	o.configMu.Lock()
	o.live = cfg
	o.configMu.Unlock()
	issue := schedulerIssue("shutdown-hook", "Done")
	path, err := o.workspaces.Create(context.Background(), issue.Identifier)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { o.cleanup(ctx, issue); close(done) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err = os.Stat(filepath.Join(path, "hook-started")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(filepath.Join(path, "hook-started"))
	if err != nil {
		t.Fatal("cleanup hook never started", err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal cleanup ignored host cancellation")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("terminal workspace not removed", err)
	}
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("cleanup hook process survived", err)
	}
}
