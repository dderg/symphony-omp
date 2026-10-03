package symphony

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type WorkspaceManager struct {
	Config  func() Config
	Logger  *slog.Logger
	Secrets []string
}

func WorkspaceKey(identifier string) string {
	var b strings.Builder
	changed := false
	for _, r := range identifier {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
			changed = true
		}
	}
	if changed {
		sum := sha256.Sum256([]byte(identifier))
		fmt.Fprintf(&b, "_%x", sum[:8])
	}
	return b.String()
}
func physicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := absolute
	parts := []string{}
	for {
		physical, e := filepath.EvalSymlinks(ancestor)
		if e == nil {
			for n := len(parts) - 1; n >= 0; n-- {
				physical = filepath.Join(physical, parts[n])
			}
			return physical, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", e
		}
		parts = append(parts, filepath.Base(ancestor))
		ancestor = parent
	}
}
func (m *WorkspaceManager) path(identifier string) (string, error) {
	key := WorkspaceKey(identifier)
	if key == "" || key == "." || key == ".." {
		return "", fault("invalid_workspace_cwd", "invalid workspace identifier")
	}
	root := m.Config().WorkspaceRoot
	var err error
	root, err = physicalPath(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, key)
	if err := m.Validate(path); err != nil {
		return "", err
	}
	return path, nil
}
func (m *WorkspaceManager) Validate(path string) error {
	root, err := filepath.Abs(m.Config().WorkspaceRoot)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// A trusted root can use platform aliases; issue-controlled entries cannot.
	if info, e := os.Lstat(absolute); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return fault("invalid_workspace_cwd", "symlink workspace")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	root, err = physicalPath(root)
	if err != nil {
		return err
	}
	parent, e := physicalPath(filepath.Dir(absolute))
	if e != nil {
		return e
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	rel, err := filepath.Rel(root, absolute)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fault("invalid_workspace_cwd", "workspace is not a child of physical root")
	}
	for p := absolute; p != root; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return fault("invalid_workspace_cwd", "symlink in workspace path")
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}
func (m *WorkspaceManager) Create(ctx context.Context, identifier string) (string, error) {
	path, err := m.path(identifier)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(m.Config().WorkspaceRoot, 0700); err != nil {
		return "", err
	}
	path, err = m.path(identifier)
	if err != nil {
		return "", err
	}
	err = os.Mkdir(path, 0700)
	if os.IsExist(err) {
		info, e := os.Lstat(path)
		if e != nil {
			return path, e
		}
		if !info.IsDir() {
			return path, fault("invalid_workspace_cwd", "workspace exists but is not a directory")
		}
		return path, m.Validate(path)
	}
	if err != nil {
		return "", err
	}
	// Return the created path on hook failure so the attempt can still run after_run.
	return path, m.Hook(ctx, "after_create", path)
}
func (m *WorkspaceManager) Hook(ctx context.Context, name, path string) (err error) {
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	defer func() {
		if err != nil {
			logger.Warn("hook outcome=failed", "hook", name, "workspace", path, "reason", err.Error())
		}
	}()
	c := m.Config()
	var script string
	switch name {
	case "after_create":
		script = c.Hooks.AfterCreate
	case "before_run":
		script = c.Hooks.BeforeRun
	case "after_run":
		script = c.Hooks.AfterRun
	case "before_remove":
		script = c.Hooks.BeforeRemove
	default:
		return fmt.Errorf("unknown hook %s", name)
	}
	if script == "" {
		return nil
	}
	if err := m.Validate(path); err != nil {
		return err
	}
	logger.Info("hook outcome=starting", "hook", name, "workspace", path)
	runCtx, cancel := context.WithTimeout(ctx, c.Hooks.Timeout)
	defer cancel()
	command := exec.Command("bash", "-lc", script)
	command.Dir = path
	command.Env = childEnvironment(m.Secrets)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Hook output may contain host credentials; retain only safe exit diagnostics.
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err = <-done:
	case <-runCtx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		err = runCtx.Err()
	}
	if err != nil {
		return fmt.Errorf("%s hook: %w", name, err)
	}
	logger.Info("hook outcome=completed", "hook", name)
	return nil
}
func (m *WorkspaceManager) Remove(ctx context.Context, identifier string) error {
	path, err := m.path(identifier)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fault("invalid_workspace_cwd", "refusing removal of non-directory workspace")
	}
	_ = m.Hook(ctx, "before_remove", path)
	if err = m.Validate(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}
func childEnvironment(secrets []string) []string {
	deny := map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true}
	for _, name := range secrets {
		deny[name] = true
	}
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !deny[name] {
			env = append(env, entry)
		}
	}
	return env
}

// Cancellation still runs after_run, but its detached grace fits host shutdown.
// Hooks entered before cancellation retain cancellation from their host context.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	}
	return context.WithCancel(ctx)
}
