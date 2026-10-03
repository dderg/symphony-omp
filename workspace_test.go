package symphony

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func workspaceManager(t *testing.T) *WorkspaceManager {
	t.Helper()
	c := Config{WorkspaceRoot: t.TempDir(), Hooks: Hooks{Timeout: time.Second}}
	return &WorkspaceManager{Config: func() Config { return c }, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}
func TestWorkspaceKeys(t *testing.T) {
	if WorkspaceKey("safe._-1") != "safe._-1" {
		t.Fatal("unchanged identifier modified")
	}
	a, b := WorkspaceKey("a/b"), WorkspaceKey("a#b")
	if a == b || !strings.HasPrefix(a, "a_b_") || len(a) < len("a_b_")+16 {
		t.Fatalf("bad keys %q %q", a, b)
	}
	if WorkspaceKey("a/b") != a {
		t.Fatal("non deterministic")
	}
}
func TestWorkspaceLifecycle(t *testing.T) {
	m := workspaceManager(t)
	c := m.Config()
	c.Hooks.AfterCreate = "echo created >> lifecycle"
	c.Hooks.BeforeRun = "echo before >> lifecycle"
	c.Hooks.AfterRun = "echo after >> lifecycle"
	c.Hooks.BeforeRemove = "echo removed >> lifecycle; exit 1"
	m.Config = func() Config { return c }
	path, err := m.Create(context.Background(), "org/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	path2, err := m.Create(context.Background(), "org/repo#1")
	if err != nil || path != path2 {
		t.Fatalf("reuse %s %v", path2, err)
	}
	if err = m.Hook(context.Background(), "before_run", path); err != nil {
		t.Fatal(err)
	}
	if err = m.Hook(context.Background(), "after_run", path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "lifecycle"))
	if err != nil || string(data) != "created\nbefore\nafter\n" {
		t.Fatalf("%q %v", data, err)
	}
	if err = m.Remove(context.Background(), "org/repo#1"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
}
func TestWorkspaceFailureAndContainment(t *testing.T) {
	m := workspaceManager(t)
	c := m.Config()
	c.Hooks.AfterCreate = "exit 1"
	m.Config = func() Config { return c }
	path, err := m.Create(context.Background(), "failed")
	if err == nil || path == "" {
		t.Fatalf("path must survive hook failure: %q %v", path, err)
	}
	for _, identifier := range []string{"", ".", ".."} {
		if _, err = m.Create(context.Background(), identifier); err == nil {
			t.Fatalf("accepted %q", identifier)
		}
	}
	if err = m.Validate(c.WorkspaceRoot); err == nil {
		t.Fatal("accepted root")
	}
	if err = m.Validate(filepath.Join(c.WorkspaceRoot, "..", "escape")); err == nil {
		t.Fatal("accepted escape")
	}
	file := filepath.Join(c.WorkspaceRoot, "file")
	if err = os.WriteFile(file, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Create(context.Background(), "file"); err == nil {
		t.Fatal("accepted file")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "preserve" {
		t.Fatal("destroyed file")
	}
	external := t.TempDir()
	link := filepath.Join(c.WorkspaceRoot, "link")
	if err = os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Create(context.Background(), "link"); err == nil {
		t.Fatal("accepted symlink")
	}
	if err = m.Remove(context.Background(), "link"); err == nil {
		t.Fatal("removed symlink")
	}
}
func TestWorkspaceTrustedRootAliases(t *testing.T) {
	physical := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	m := workspaceManager(t)
	c := m.Config()
	c.WorkspaceRoot = filepath.Join(alias, "new-root")
	m.Config = func() Config { return c }
	path, err := m.Create(context.Background(), "safe")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(filepath.Join(c.WorkspaceRoot, "safe"))
	if err != nil || path != expected {
		t.Fatalf("physical cwd %s want %s (%v)", path, expected, err)
	}
	if err = m.Remove(context.Background(), "safe"); err != nil {
		t.Fatal(err)
	}
}
func TestWorkspaceDefaultTempRoot(t *testing.T) {
	c, err := parseConfig(map[string]any{"tracker": map[string]any{"kind": "github_projects"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := workspaceManager(t)
	m.Config = func() Config { return c }
	identifier := "symphony-default-temp-test-" + filepath.Base(t.TempDir())
	path, err := m.Create(context.Background(), identifier)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Remove(context.Background(), identifier) })
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		t.Fatalf("%s %s %v", path, physical, err)
	}
}
func TestHooksTimeoutAndSecretIsolation(t *testing.T) {
	m := workspaceManager(t)
	c := m.Config()
	c.Hooks.Timeout = 30 * time.Millisecond
	c.Hooks.BeforeRun = "sleep 5"
	m.Config = func() Config { return c }
	path, err := m.Create(context.Background(), "timeout")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err = m.Hook(context.Background(), "before_run", path); err == nil || time.Since(started) > time.Second {
		t.Fatalf("timeout failed %v", err)
	}
	t.Setenv("CUSTOM_TRACKER_SECRET", "private")
	m.Secrets = []string{"CUSTOM_TRACKER_SECRET"}
	c.Hooks.BeforeRun = `test -z "$CUSTOM_TRACKER_SECRET"`
	if err = m.Hook(context.Background(), "before_run", path); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoredHooksLogValidationAndStartFailures(t *testing.T) {
	m := workspaceManager(t)
	c := m.Config()
	c.Hooks.AfterRun = "true"
	m.Config = func() Config { return c }
	var logs bytes.Buffer
	m.Logger = slog.New(slog.NewJSONHandler(&logs, nil)).With("issue_id", "item", "issue_identifier", "org/repo#1")
	file := filepath.Join(c.WorkspaceRoot, "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_ = m.Hook(context.Background(), "after_run", file)
	_ = m.Hook(context.Background(), "after_run", t.TempDir())
	warnings := 0
	for _, line := range bytes.Split(logs.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record["level"] == "WARN" {
			warnings++
			if record["hook"] != "after_run" || record["issue_id"] != "item" || record["reason"] == nil {
				t.Fatalf("missing warning context: %#v", record)
			}
		}
	}
	if warnings != 2 {
		t.Fatalf("ignored validation/start failures produced %d warnings", warnings)
	}
}
