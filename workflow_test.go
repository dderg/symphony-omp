package symphony

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testIssue() Issue {
	return Issue{ID: "item", Identifier: "org/repo#1", Title: "Task", State: "Todo", Labels: []string{}, BlockedBy: []Blocker{}, Dispatchable: true}
}
func TestStrictTemplates(t *testing.T) {
	issue := testIssue()
	attempt := 2
	cases := []struct {
		source, want string
		bad          bool
		attempt      *int
	}{
		{`{{ issue.description }}|{{ attempt }}`, "|", false, nil},
		{`{% if issue.description == nil %}none{% endif %}`, "none", false, nil},
		{`{% if nil == issue.description %}none{% endif %}`, "none", false, nil},
		{`{{ issue.identifier }} {{ attempt }}`, "org/repo#1 2", false, &attempt},
		{`{{ issue.missing }}`, "", true, nil},
		{`{% if issue.missing %}x{% endif %}`, "", true, nil},
		{`{% if false %}{{ issue.missing }}{% endif %}`, "", true, nil},
		{`{{ issue.priority.missing }}`, "", true, nil},
		{`{{ issue["missing"] }}`, "", true, nil},
		{`{% if false %}{{ issue["missing"] }}{% endif %}`, "", true, nil},
		{`{% for b in issue.blocked_by %}{{ b.missing }}{% endfor %}`, "", true, nil},
		{`{{ forloop.index }}`, "", true, nil},
		{`{% for label in issue.labels %}{{ forloop.index }}{{ label }}{% endfor %}`, "", false, nil},
		{`{% for label    in    issue.labels %}{{ forloop.index }}{{ label }}{% endfor %}`, "", false, nil},
		{`{% for label in issue.labels %}{{ label }}{% endfor %}{{ label }}`, "", true, nil},
		{`{% if false %}{{ issue.title | nonexistent }}{% endif %}`, "", true, nil},
		{`{{ issue.title | upcase }}`, "TASK", false, nil},
		{`{% assign copied = issue %}{{ copied.title }}`, "Task", false, nil},
		{`{% assign title = issue.title | upcase %}{{ title }}`, "TASK", false, nil},
		{`{% if false %}{% assign branch_only = 1 %}{% endif %}{% if branch_only %}bad{% endif %}`, "", true, nil},
		{`{% if issue.title %}{% assign assigned = issue.title %}{% endif %}{{ assigned }}`, "Task", false, nil},
		{`{% if false %}{% assign unused = 1 %}{% else %}{% assign assigned = issue.title %}{% endif %}{{ assigned }}`, "Task", false, nil},
		{`{% assign field = "missing" %}{% if false %}{{ issue[field] }}{% endif %}`, "", true, nil},
		{`{% if false %}{{ issue.blocked_by[0].missing }}{% endif %}`, "", true, nil},
		{`{% raw %}{{ missing }}{% endraw %}`, "{{ missing }}", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			got, err := RenderPrompt(&Workflow{Prompt: tc.source}, issue, tc.attempt)
			if tc.bad {
				if err == nil {
					t.Fatalf("expected error; got %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
func TestTemplateNestedData(t *testing.T) {
	issue := testIssue()
	id := "blocking"
	issue.BlockedBy = []Blocker{{ID: &id}}
	issue.NativeRef = map[string]any{"nested": map[string]any{"value": "yes"}}
	issue.Labels = []string{"a", "b"}
	source := `{% for label in issue.labels %}{{ forloop.index }}={{ label }};{% endfor %}{% for b in issue.blocked_by %}{{ b.id }}{{ b.state }}{% endfor %}{{ issue.native_ref.nested.value }}`
	got, err := RenderPrompt(&Workflow{Prompt: source}, issue, nil)
	if err != nil || got != "1=a;2=b;blockingyes" {
		t.Fatalf("got %q %v", got, err)
	}
}
func TestWorkflowLoading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	t.Setenv("WORKSPACE_TEST", filepath.Join(dir, "env-root"))
	source := "---\ntracker:\n  kind: github_projects\n  provider:\n    arbitrary: retained\nworkspace:\n  root: $WORKSPACE_TEST\nagent:\n  max_concurrent_agents_by_state:\n    ' In Progress ': 2\n    invalid: -1\nomp:\n  command: 'printf \"$HOME\"'\n  stall_timeout_ms: -9223372036854775808\n---\n  Task {{ issue.title }}  \n"
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	if w.Prompt != "Task {{ issue.title }}" || w.Config.Command != `printf "$HOME"` || w.Config.WorkspaceRoot != os.Getenv("WORKSPACE_TEST") || w.Config.StallTimeout != 0 || w.Config.ByState["in progress"] != 2 || len(w.Config.ByState) != 1 || w.Config.Tracker.Provider["arbitrary"] != "retained" {
		t.Fatalf("bad config %#v", w)
	}
	if w.Config.PollInterval != 30*time.Second || w.Config.MaxTurns != 20 || w.Config.Hooks.Timeout != time.Minute {
		t.Fatal("defaults missing")
	}
	for _, tc := range []struct{ body, category string }{{"---\n[one, two]\n---\nprompt", "workflow_front_matter_not_a_map"}, {"---\na: [\n---\nx", "workflow_parse_error"}, {"---\na: b", "workflow_parse_error"}} {
		if err = os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err = LoadWorkflow(path)
		var e *Error
		if !errors.As(err, &e) || e.Category != tc.category {
			t.Fatalf("got %v want %s", err, tc.category)
		}
	}
	_, err = LoadWorkflow(filepath.Join(dir, "absent"))
	var e *Error
	if !errors.As(err, &e) || e.Category != "missing_workflow_file" {
		t.Fatal(err)
	}
}
func TestConfigPathsAndValidation(t *testing.T) {
	dir := t.TempDir()
	base := map[string]any{"tracker": map[string]any{"kind": "github_projects"}, "workspace": map[string]any{"root": "relative"}}
	c, err := parseConfig(base, dir)
	if err != nil || c.WorkspaceRoot != filepath.Join(dir, "relative") {
		t.Fatalf("%#v %v", c, err)
	}
	home, _ := os.UserHomeDir()
	base["workspace"] = map[string]any{"root": "~/workspaces"}
	c, err = parseConfig(base, dir)
	if err != nil || c.WorkspaceRoot != filepath.Join(home, "workspaces") {
		t.Fatalf("%s %v", c.WorkspaceRoot, err)
	}
	base["hooks"] = map[string]any{"timeout_ms": 0}
	_, err = parseConfig(base, dir)
	if err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Fatal(err)
	}
}
