package symphony

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Hooks struct {
	AfterCreate, BeforeRun, AfterRun, BeforeRemove string
	Timeout                                        time.Duration
}
type Config struct {
	Tracker                                TrackerConfig
	PollInterval                           time.Duration
	WorkspaceRoot                          string
	Hooks                                  Hooks
	MaxConcurrent, MaxTurns                int
	MaxBackoff                             time.Duration
	ByState                                map[string]int
	Command                                string
	ReadTimeout, TurnTimeout, StallTimeout time.Duration
}
type Workflow struct {
	Path   string
	Source []byte
	Prompt string
	Config Config
}

func LoadWorkflow(path string) (*Workflow, error) {
	if path == "" {
		path = "WORKFLOW.md"
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	source, err := os.ReadFile(absolute)
	if err != nil {
		return nil, fault("missing_workflow_file", err.Error())
	}
	body := string(source)
	raw := map[string]any{}
	lines := strings.Split(body, "\n")
	if strings.TrimSuffix(lines[0], "\r") == "---" {
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSuffix(lines[i], "\r") == "---" {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, fault("workflow_parse_error", "unclosed YAML front matter")
		}
		var node yaml.Node
		decoder := yaml.NewDecoder(strings.NewReader(strings.Join(lines[1:end], "\n")))
		if err = decoder.Decode(&node); err != nil {
			return nil, fault("workflow_parse_error", err.Error())
		}
		if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
			return nil, fault("workflow_front_matter_not_a_map", "front matter must be an object")
		}
		if err = node.Decode(&raw); err != nil {
			return nil, fault("workflow_parse_error", err.Error())
		}
		body = strings.Join(lines[end+1:], "\n")
	}
	config, err := parseConfig(raw, filepath.Dir(absolute))
	if err != nil {
		return nil, err
	}
	return &Workflow{Path: absolute, Source: source, Prompt: strings.TrimSpace(body), Config: config}, nil
}

func parseConfig(raw map[string]any, dir string) (Config, error) {
	c := Config{PollInterval: 30 * time.Second, WorkspaceRoot: filepath.Join(os.TempDir(), "symphony_workspaces"), Hooks: Hooks{Timeout: time.Minute}, MaxConcurrent: 10, MaxTurns: 20, MaxBackoff: 5 * time.Minute, ByState: map[string]int{}, Command: "omp --mode rpc --no-ui", ReadTimeout: 5 * time.Second, TurnTimeout: time.Hour, StallTimeout: 5 * time.Minute}
	sections := map[string]map[string]any{}
	for _, name := range []string{"tracker", "polling", "workspace", "hooks", "agent", "omp"} {
		if v, ok := raw[name]; ok {
			m, yes := v.(map[string]any)
			if !yes {
				return c, fault("invalid_config", name+" must be an object")
			}
			sections[name] = m
		} else {
			sections[name] = map[string]any{}
		}
	}
	var err error
	stringValue := func(m map[string]any, key string, dst *string) {
		if v, ok := m[key]; ok {
			s, yes := v.(string)
			if !yes {
				err = fmt.Errorf("%s must be a string", key)
			} else {
				*dst = s
			}
		}
	}
	intValue := func(m map[string]any, key string, dst *int, positive bool) {
		if v, ok := m[key]; ok {
			n, e := integer(v)
			if e != nil || (positive && n <= 0) {
				err = fmt.Errorf("%s must be a positive integer", key)
			} else {
				*dst = n
			}
		}
	}
	durationValue := func(m map[string]any, key string, dst *time.Duration, positive bool) {
		n := int(*dst / time.Millisecond)
		intValue(m, key, &n, positive)
		if !positive && n <= 0 {
			*dst = 0
			return
		}
		if n > int((1<<63-1)/int64(time.Millisecond)) {
			err = fmt.Errorf("%s duration overflows", key)
		} else {
			*dst = time.Duration(n) * time.Millisecond
		}
	}
	t := sections["tracker"]
	stringValue(t, "kind", &c.Tracker.Kind)
	c.Tracker.Provider = map[string]any{}
	if v, ok := t["provider"]; ok {
		var yes bool
		c.Tracker.Provider, yes = v.(map[string]any)
		if !yes {
			err = fmt.Errorf("tracker.provider must be an object")
		}
	}
	c.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	c.Tracker.TerminalStates = []string{"Done"}
	c.Tracker.RequiredLabels = []string{}
	for key, dst := range map[string]*[]string{"active_states": &c.Tracker.ActiveStates, "terminal_states": &c.Tracker.TerminalStates, "required_labels": &c.Tracker.RequiredLabels} {
		if v, ok := t[key]; ok {
			a, yes := v.([]any)
			if !yes {
				err = fmt.Errorf("tracker.%s must be a string list", key)
				continue
			}
			*dst = []string{}
			for _, item := range a {
				s, yes := item.(string)
				if !yes {
					err = fmt.Errorf("tracker.%s must contain strings", key)
				} else {
					*dst = append(*dst, s)
				}
			}
		}
	}
	durationValue(sections["polling"], "interval_ms", &c.PollInterval, true)
	stringValue(sections["workspace"], "root", &c.WorkspaceRoot)
	if strings.HasPrefix(c.WorkspaceRoot, "$") {
		c.WorkspaceRoot = os.Getenv(strings.TrimPrefix(c.WorkspaceRoot, "$"))
		if c.WorkspaceRoot == "" {
			err = fmt.Errorf("workspace.root environment reference is empty")
		}
	}
	if c.WorkspaceRoot == "~" || strings.HasPrefix(c.WorkspaceRoot, "~/") {
		home, e := os.UserHomeDir()
		if e != nil {
			err = e
		} else {
			c.WorkspaceRoot = filepath.Join(home, strings.TrimPrefix(c.WorkspaceRoot, "~"))
		}
	}
	if !filepath.IsAbs(c.WorkspaceRoot) {
		c.WorkspaceRoot = filepath.Join(dir, c.WorkspaceRoot)
	}
	c.WorkspaceRoot = filepath.Clean(c.WorkspaceRoot)
	h := sections["hooks"]
	stringValue(h, "after_create", &c.Hooks.AfterCreate)
	stringValue(h, "before_run", &c.Hooks.BeforeRun)
	stringValue(h, "after_run", &c.Hooks.AfterRun)
	stringValue(h, "before_remove", &c.Hooks.BeforeRemove)
	durationValue(h, "timeout_ms", &c.Hooks.Timeout, true)
	a := sections["agent"]
	intValue(a, "max_concurrent_agents", &c.MaxConcurrent, true)
	intValue(a, "max_turns", &c.MaxTurns, true)
	durationValue(a, "max_retry_backoff_ms", &c.MaxBackoff, true)
	if m, ok := a["max_concurrent_agents_by_state"].(map[string]any); ok {
		for k, v := range m {
			n, e := integer(v)
			if e == nil && n > 0 {
				c.ByState[normalize(k)] = n
			}
		}
	}
	o := sections["omp"]
	stringValue(o, "command", &c.Command)
	durationValue(o, "read_timeout_ms", &c.ReadTimeout, true)
	durationValue(o, "turn_timeout_ms", &c.TurnTimeout, true)
	durationValue(o, "stall_timeout_ms", &c.StallTimeout, false)
	if err != nil {
		return c, fault("invalid_config", err.Error())
	}
	if strings.TrimSpace(c.Command) == "" {
		return c, fault("invalid_config", "omp.command must be non-empty")
	}
	if c.Tracker.Kind != "github_projects" {
		return c, fault("unsupported_tracker_kind", "supported kind is github_projects")
	}
	return c, nil
}
func integer(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case string:
		return strconv.Atoi(n)
	default:
		return 0, fmt.Errorf("not integer")
	}
}
func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func containsState(states []string, state string) bool {
	for _, s := range states {
		if normalize(s) == normalize(state) {
			return true
		}
	}
	return false
}
func routable(c Config, i Issue) bool {
	if !i.Dispatchable {
		return false
	}
	for _, want := range c.Tracker.RequiredLabels {
		found := false
		for _, got := range i.Labels {
			if normalize(want) != "" && normalize(want) == normalize(got) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func NewTracker(ctx context.Context, cfg TrackerConfig, logger *slog.Logger) (Tracker, error) {
	if cfg.Kind != "github_projects" {
		return nil, fault("unsupported_tracker_kind", cfg.Kind)
	}
	return NewGitHubProjects(ctx, cfg, nil, os.LookupEnv, logger)
}
