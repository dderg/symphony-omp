---
tracker:
  kind: github_projects
  provider:
    project_id: $GITHUB_PROJECT_ID
    api_key: $GITHUB_TOKEN
    status_field: Status
  active_states: [Todo, In Progress]
  terminal_states: [Done]
  required_labels: []
polling:
  interval_ms: 30000
workspace:
  root: ./symphony_workspaces
hooks:
  after_create: |
    : "${SYMPHONY_REPOSITORY_URL:?Set a trusted repository URL on the host}"
    git clone -- "$SYMPHONY_REPOSITORY_URL" .
  timeout_ms: 60000
agent:
  max_concurrent_agents: 2
  max_turns: 20
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state: {}
omp:
  command: omp --mode rpc --no-ui
  read_timeout_ms: 5000
  turn_timeout_ms: 3600000
  stall_timeout_ms: 300000
---
Implement GitHub issue {{ issue.identifier }}: {{ issue.title }}.

{{ issue.description }}

{% if attempt != nil %}
This is retry/continuation attempt {{ attempt }}. Inspect the preserved workspace and
current issue before repeating commands or mutations.
{% endif %}

Read the repository's instructions before editing. Work only in this issue's workspace.
Use the host-side github_get_issue tool to check the current issue and board Status;
do not request or search for raw tracker credentials.

Implement the complete requested behavior and run the relevant checks. Do not commit,
push, open a pull request or land changes unless the issue or repository workflow
explicitly requires it. Do not perform unrelated cleanup.

When work is complete, use github_add_comment to report touched files, exercised checks
and any constraints, then use github_set_status with status "Human Review". This is a
non-active, non-terminal handoff that preserves local changes for review. Never claim an
unexercised check passed. If blocked or awaiting human input, report the exact blocker
instead of claiming completion. Do not set "Done": terminal cleanup deletes this workspace,
including uncommitted work. Completed artifacts must be preserved externally before an
operator moves the issue to a terminal state. Board Status and issue OPEN/CLOSED are distinct;
this workflow does not require closing the underlying issue.
