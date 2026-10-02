# Symphony Service Specification

Status: Draft v1 (language-agnostic)

Purpose: Define a service that orchestrates coding agents to get project work done.

## Normative Language

The key words `MUST`, `MUST NOT`, `REQUIRED`, `SHOULD`, `SHOULD NOT`, `RECOMMENDED`, `MAY`, and
`OPTIONAL` in this document are to be interpreted as described in RFC 2119.

`Implementation-defined` means the behavior is part of the implementation contract, but this
specification does not prescribe one universal policy. Implementations MUST document the selected
behavior.

## 1. Problem Statement

Symphony is a long-running automation service that continuously reads work from a configured
GitHub Projects board, creates an isolated workspace for each issue, and runs an oh-my-pi coding
agent session for that issue inside the workspace.

The service solves four operational problems:

- It turns issue execution into a repeatable daemon workflow instead of manual scripts.
- It isolates agent execution in per-issue workspaces so agent commands run only inside per-issue
  workspace directories.
- It keeps the workflow policy in-repo (`WORKFLOW.md`) so teams version the agent prompt and runtime
  settings with their code.
- It provides enough observability to operate and debug multiple concurrent agent runs.

Implementations are expected to document their trust and safety posture explicitly. This
specification does not require a single approval, sandbox, or operator-confirmation policy; some
implementations target trusted environments with a high-trust configuration, while others require
stricter approvals or sandboxing.

Important boundary:

- Symphony is a scheduler/runner and tracker reader.
- Ticket writes (state transitions, comments, PR links) are typically performed by the coding agent
  through provider-native tools executed by Symphony with the configured tracker credential.
- When tracker credentials are supplied through host-side secret references, the coding-agent child
  process does not need a duplicate tracker login or direct access to raw tracker credentials.
- A successful run can end at a workflow-defined handoff state (for example `Human Review`), not
  necessarily `Done`.

## 2. Goals and Non-Goals

### 2.1 Goals

- Poll the issue tracker on a fixed cadence and dispatch work with bounded concurrency.
- Maintain a single authoritative orchestrator state for dispatch, retries, and reconciliation.
- Create deterministic per-issue workspaces and preserve them across runs.
- Stop active runs when issue state changes make them ineligible.
- Recover from transient failures with exponential backoff.
- Load runtime behavior from a repository-owned `WORKFLOW.md` contract.
- Expose operator-visible observability (at minimum structured logs).
- Support tracker/filesystem-driven restart recovery without requiring a persistent database; exact
  in-memory scheduler state is not restored.

### 2.2 Non-Goals

- Rich web UI or multi-tenant control plane.
- Prescribing a specific dashboard or terminal UI implementation.
- General-purpose workflow engine or distributed job scheduler.
- Built-in business logic for how to edit tickets, PRs, or comments. (That logic lives in the
  workflow prompt and agent tooling.)
- Mandating strong sandbox controls beyond what the coding agent and host OS provide.
- Mandating a single default approval, sandbox, or operator-confirmation posture for all
  implementations.

## 3. System Overview

### 3.1 Main Components

1. `Workflow Loader`
   - Reads `WORKFLOW.md`.
   - Parses YAML front matter and prompt body.
   - Returns `{config, prompt_template}`.

2. `Config Layer`
   - Exposes typed getters for workflow config values.
   - Applies defaults and environment variable indirection.
   - Performs validation used by the orchestrator before dispatch.

3. `Issue Tracker Adapter`
   - Fetches candidate issues in active states.
   - Fetches current states for specific issue IDs (reconciliation).
   - Fetches terminal-state issues during startup cleanup.
   - Normalizes tracker payloads into a stable issue model.
   - MAY expose provider-native agent tools without adding provider-specific write APIs to the
     orchestrator.

4. `Orchestrator`
   - Owns the poll tick.
   - Owns the in-memory runtime state.
   - Decides which issues to dispatch, retry, stop, or release.
   - Tracks session metrics and retry queue state.

5. `Workspace Manager`
   - Maps issue identifiers to workspace paths.
   - Ensures per-issue workspace directories exist.
   - Runs workspace lifecycle hooks.
   - Cleans workspaces for terminal issues.

6. `Agent Runner`
   - Creates workspace.
   - Builds prompt from issue + workflow template.
   - Launches the oh-my-pi RPC client.
   - Streams agent updates back to the orchestrator.

7. `Status Surface` (OPTIONAL)
   - Presents human-readable runtime status (for example terminal output, dashboard, or other
     operator-facing view).

8. `Logging`
   - Emits structured runtime logs to one or more configured sinks.

### 3.2 Abstraction Levels

Symphony is easiest to port when kept in these layers:

1. `Policy Layer` (repo-defined)
   - `WORKFLOW.md` prompt body.
   - Team-specific rules for ticket handling, validation, and handoff.

2. `Configuration Layer` (typed getters)
   - Parses front matter into typed runtime settings.
   - Handles defaults, environment tokens, and path normalization.

3. `Coordination Layer` (orchestrator)
   - Polling loop, issue eligibility, concurrency, retries, reconciliation.

4. `Execution Layer` (workspace + agent subprocess)
   - Filesystem lifecycle, workspace preparation, coding-agent protocol.

5. `Integration Layer` (selected tracker adapter)
   - API calls and normalization for tracker data.
   - Provider-native agent tools and centralized tracker authentication.

6. `Observability Layer` (logs + OPTIONAL status surface)
   - Operator visibility into orchestrator and agent behavior.

### 3.3 External Dependencies

- GitHub GraphQL API for a configured GitHub Projects (`ProjectV2`) board.
- Local filesystem for workspaces and logs.
- OPTIONAL workspace population tooling (for example Git CLI, if used).
- oh-my-pi executable (`omp`) that supports the targeted RPC mode.
- Host environment authentication for the issue tracker and coding agent. Host-side tracker secret
  environment variables SHOULD NOT be inherited by the coding-agent child process.

## 4. Core Domain Model

### 4.1 Entities

#### 4.1.1 Issue

Normalized schedulable work item used by orchestration, prompt rendering, and observability output.
The name `Issue` is generic in this specification; an adapter MAY map it from a ticket, card,
project item, or another provider-native work object.

Fields:

- `id` (string)
  - REQUIRED stable dispatch identity within the configured tracker scope.
  - Opaque to the orchestrator. It MAY be a project-item or board-entry ID instead of the
    provider's underlying ticket ID.
- `native_ref` (object or null)
  - OPTIONAL non-secret provider identifiers needed by provider-native tools.
  - Opaque to the orchestrator and preserved for prompt/tool context.
- `identifier` (string)
  - REQUIRED human-readable ticket key (example: `octo-org/example#123`).
  - MUST be unique within the configured tracker scope because it names workspaces and
    operator-facing routes. An adapter spanning multiple namespaces MUST disambiguate it.
- `title` (string)
- `description` (string or null)
- `priority` (integer or null)
  - Lower numbers are higher priority in dispatch sorting.
- `state` (string)
  - REQUIRED current provider-native state name.
- `branch_name` (string or null)
  - Tracker-provided branch metadata if available.
- `url` (string or null)
- `assignee_id` (string or null)
- `labels` (list of strings)
  - Normalized to lowercase.
- `blocked_by` (list of blocker refs)
  - Best-effort provider metadata. Each blocker ref contains:
    - `id` (string or null)
    - `identifier` (string or null)
    - `state` (string or null)
- `dispatchable` (boolean)
  - REQUIRED adapter-derived eligibility for provider-specific rules that the generic scheduler
    cannot infer safely, such as assignment, board membership, or blocker semantics.
  - The orchestrator still applies configured state, label, claim, retry, and concurrency rules.
- `created_at` (timestamp or null)
- `updated_at` (timestamp or null)

#### 4.1.2 Workflow Definition

Parsed `WORKFLOW.md` payload:

- `config` (map)
  - YAML front matter root object.
- `prompt_template` (string)
  - Markdown body after front matter, trimmed.

#### 4.1.3 Service Config (Typed View)

Typed runtime values derived from `WorkflowDefinition.config` plus environment resolution.

Examples:

- poll interval
- workspace root
- active and terminal issue states
- concurrency limits
- coding-agent executable/args/timeouts
- workspace hooks

#### 4.1.4 Workspace

Filesystem workspace assigned to one issue identifier.

Fields (logical):

- `path` (absolute workspace path)
- `workspace_key` (collision-resistant sanitized issue identifier)
- `created_now` (boolean, used to gate `after_create` hook)

#### 4.1.5 Run Attempt

One execution attempt for one issue.

Fields (logical):

- `issue_id`
- `issue_identifier`
- `attempt` (integer or null, `null` for first run, `>=1` for retries/continuation)
- `workspace_path`
- `started_at`
- `status`
- `error` (OPTIONAL)

#### 4.1.6 Live Session (Agent Session Metadata)

State tracked while a coding-agent subprocess is running.

Fields:

- `session_id` (string, `<omp_session_id>-<turn_id>`)
- `omp_session_id` (string, from RPC `get_state.data.sessionId`)
- `turn_id` (string, host-generated `prompt` request ID)
- `omp_pid` (string or null)
- `last_omp_event` (string/enum or null)
- `last_omp_timestamp` (timestamp or null)
- `last_omp_message` (summarized payload)
- `omp_input_tokens` (integer)
- `omp_output_tokens` (integer)
- `omp_total_tokens` (integer)
- `last_reported_input_tokens` (integer)
- `last_reported_output_tokens` (integer)
- `last_reported_total_tokens` (integer)
- `turn_count` (integer)
  - Number of coding-agent turns started within the current worker lifetime.

#### 4.1.7 Retry Entry

Scheduled retry state for an issue.

Fields:

- `issue_id`
- `identifier` (best-effort human ID for status surfaces/logs)
- `attempt` (integer, 1-based for retry queue)
- `due_at_ms` (monotonic clock timestamp)
- `timer_handle` (runtime-specific timer reference)
- `error` (string or null)

#### 4.1.8 Orchestrator Runtime State

Single authoritative in-memory state owned by the orchestrator.

Fields:

- `poll_interval_ms` (current effective poll interval)
- `max_concurrent_agents` (current effective global concurrency limit)
- `running` (map `issue_id -> running entry`)
- `claimed` (set of issue IDs reserved/running/retrying)
- `retry_attempts` (map `issue_id -> RetryEntry`)
- `completed` (set of issue IDs; bookkeeping only, not dispatch gating)
- `omp_totals` (aggregate tokens + runtime seconds)
- `omp_rate_limits` (latest provider rate-limit snapshot, or null when unavailable)

### 4.2 Stable Identifiers and Normalization Rules

- `Issue ID`
  - Use for tracker refresh calls and internal map keys.
  - Treat it as an opaque dispatch identity; do not assume it is the provider's underlying ticket
    ID.
- `Native Ref`
  - Preserve as opaque non-secret data for provider-native agent tools and prompt rendering.
  - Never use it as an orchestrator map key or interpret provider-specific fields in core logic.
- `Issue Identifier`
  - Use for human-readable logs and workspace naming.
  - Require uniqueness within the configured tracker scope.
- `Workspace Key`
  - Derive from `issue.identifier` by replacing any character not in `[A-Za-z0-9._-]` with `_`.
  - If sanitization changes the identifier, append a stable hash suffix of the original identifier
    with at least 64 bits of entropy using only allowed workspace-key characters, making keys for
    distinct identifiers that sanitize to the same text collision-resistant.
  - Use the resulting value for the workspace directory name.
- `Normalized Issue State`
  - Compare states after trimming surrounding whitespace and applying `lowercase`.
- `Session ID`
  - Compose from `omp_session_id` and the host-generated `turn_id` as
    `<omp_session_id>-<turn_id>`.

## 5. Workflow Specification (Repository Contract)

### 5.1 File Discovery and Path Resolution

Workflow file path precedence:

1. Explicit application/runtime setting (set by CLI startup path).
2. Default: `WORKFLOW.md` in the current process working directory.

Loader behavior:

- If the file cannot be read, return `missing_workflow_file` error.
- The workflow file is expected to be repository-owned and version-controlled.

### 5.2 File Format

`WORKFLOW.md` is a Markdown file with OPTIONAL YAML front matter.

Design note:

- `WORKFLOW.md` SHOULD be self-contained enough to describe and run different workflows (prompt,
  runtime settings, hooks, and tracker selection/config) without requiring out-of-band
  service-specific configuration.

Parsing rules:

- If file starts with `---`, parse lines until the next `---` as YAML front matter.
- Remaining lines become the prompt body.
- If front matter is absent, treat the entire file as prompt body and use an empty config map.
- YAML front matter MUST decode to a map/object; non-map YAML is an error.
- Prompt body is trimmed before use.

Returned workflow object:

- `config`: front matter root object (not nested under a `config` key).
- `prompt_template`: trimmed Markdown body.

### 5.3 Front Matter Schema

Top-level keys:

- `tracker`
- `polling`
- `workspace`
- `hooks`
- `agent`
- `omp`

Unknown keys SHOULD be ignored for forward compatibility.

Note:

- The workflow front matter is extensible. Extensions MAY define additional top-level keys without
  changing the core schema above.
- Extensions SHOULD document their field schema, defaults, validation rules, and whether changes
  apply dynamically or require restart.

#### 5.3.1 `tracker` (object)

Fields:

- `kind` (string)
  - REQUIRED for dispatch.
  - Selects one implementation-supported tracker adapter.
  - This specification REQUIRES `github_projects`, the GitHub Projects board profile in Section 11.6.
    Implementations MAY support additional adapters.
- `provider` (object)
  - Default: `{}`.
  - Adapter-owned configuration such as endpoint, scope/project selector, and credentials.
  - Core Symphony MUST preserve unknown keys and MUST NOT prescribe one cross-provider credential
    or scope schema.
  - Each adapter MUST document its required keys, defaults, secret keys, `$VAR_NAME` support, and
    validation errors.
  - If a documented secret `$VAR_NAME` resolves to an empty string, treat that secret as missing.
- `required_labels` (list of strings)
  - Default: `[]`.
  - An issue MUST contain every configured label to dispatch or continue.
  - Matching ignores case and surrounding whitespace.
  - A blank configured label matches no issue.
- `active_states` (list of strings)
  - REQUIRED unless the selected adapter profile documents a default.
  - Values are provider-native state names compared case-insensitively by the scheduler.
- `terminal_states` (list of strings)
  - REQUIRED unless the selected adapter profile documents a default.
  - Values are provider-native state names compared case-insensitively by the scheduler.

For `github_projects`, state names are options in the board's configured single-select Status
field, not the underlying GitHub issue's `OPEN`/`CLOSED` state. Defaults and provider keys are
defined in Section 11.6.

#### 5.3.2 `polling` (object)

Fields:

- `interval_ms` (integer)
  - Default: `30000`
  - Changes SHOULD be re-applied at runtime and affect future tick scheduling without restart.

#### 5.3.3 `workspace` (object)

Fields:

- `root` (path string or `$VAR`)
  - Default: `<system-temp>/symphony_workspaces`
  - `~` is expanded.
  - Relative paths are resolved relative to the directory containing `WORKFLOW.md`.
  - The effective workspace root is normalized to an absolute path before use.

#### 5.3.4 `hooks` (object)

Fields:

- `after_create` (multiline shell script string, OPTIONAL)
  - Runs only when a workspace directory is newly created.
  - Failure aborts workspace creation.
- `before_run` (multiline shell script string, OPTIONAL)
  - Runs before each agent attempt after workspace preparation and before launching the coding
    agent.
  - Failure aborts the current attempt.
- `after_run` (multiline shell script string, OPTIONAL)
  - Runs after each agent attempt (success, failure, timeout, or cancellation) once the workspace
    exists.
  - Failure is logged but ignored.
- `before_remove` (multiline shell script string, OPTIONAL)
  - Runs before workspace deletion if the directory exists.
  - Failure is logged but ignored; cleanup still proceeds.
- `timeout_ms` (integer, OPTIONAL)
  - Default: `60000`
  - Applies to all workspace hooks.
  - Invalid values fail configuration validation.
  - Changes SHOULD be re-applied at runtime for future hook executions.

#### 5.3.5 `agent` (object)

Fields:

- `max_concurrent_agents` (integer)
  - Default: `10`
  - Changes SHOULD be re-applied at runtime and affect subsequent dispatch decisions.
- `max_turns` (positive integer)
  - Default: `20`
  - Limits the number of coding-agent turns within one worker session.
  - Invalid values fail configuration validation.
- `max_retry_backoff_ms` (integer)
  - Default: `300000` (5 minutes)
  - Changes SHOULD be re-applied at runtime and affect future retry scheduling.
- `max_concurrent_agents_by_state` (map `state_name -> positive integer`)
  - Default: empty map.
  - State keys are normalized (`trim + lowercase`) for lookup.
  - Invalid entries (non-positive or non-numeric) are ignored.

#### 5.3.6 `omp` (object)

Fields:

- `command` (string shell command)
  - Default: `omp --mode rpc --no-ui`
  - The runtime launches this command via `bash -lc` in the workspace directory.
  - The launched process MUST speak the targeted oh-my-pi RPC protocol over stdio.
  - Model selection, thinking level, tool restrictions, and approval settings MAY be supplied
    through supported oh-my-pi CLI options or its native configuration.
  - The effective cwd MUST remain the issue workspace; command options MUST NOT redirect it.
  - `--no-ui` runs extensions headless; tools requiring interactive approval fail closed unless
    the deployment explicitly configures a supported non-interactive approval policy.
  - RPC mode is not an OS sandbox. Implementations MUST document host isolation and approval policy.
- `turn_timeout_ms` (integer)
  - Default: `3600000` (1 hour)
- `read_timeout_ms` (integer)
  - Default: `5000`
- `stall_timeout_ms` (integer)
  - Default: `300000` (5 minutes)
  - If `<= 0`, stall detection is disabled.

### 5.4 Prompt Template Contract

The Markdown body of `WORKFLOW.md` is the per-issue prompt template.

Rendering requirements:

- Use a strict template engine (Liquid-compatible semantics are sufficient).
- Unknown variables MUST fail rendering.
- Unknown filters MUST fail rendering.

Template input variables:

- `issue` (object)
  - Includes all normalized issue fields, including labels and blockers.
- `attempt` (integer or null)
  - `null`/absent on first attempt.
  - Integer on retry or continuation run.

Fallback prompt behavior:

- If the workflow prompt body is empty, the runtime MAY use a minimal default prompt
  (`You are working on an issue from the configured tracker.`).
- Workflow file read/parse failures are configuration/validation errors and SHOULD NOT silently fall
  back to a prompt.

### 5.5 Workflow Validation and Error Surface

Error classes:

- `missing_workflow_file`
- `workflow_parse_error`
- `workflow_front_matter_not_a_map`
- `template_parse_error` (during prompt rendering)
- `template_render_error` (unknown variable/filter, invalid interpolation)

Dispatch gating behavior:

- Workflow file read/YAML errors block new dispatches until fixed.
- Template errors fail only the affected run attempt.

## 6. Configuration Specification

### 6.1 Configuration Resolution Pipeline

Configuration is resolved in this order:

1. Select the workflow file path (explicit runtime setting, otherwise cwd default).
2. Parse YAML front matter into a raw config map.
3. Apply built-in defaults for missing OPTIONAL fields.
4. Resolve `$VAR_NAME` indirection for config values that explicitly contain `$VAR_NAME`, plus any
   adapter-owned fallback environment names documented for omitted provider fields.
5. Coerce and validate typed values.

Environment variables do not globally override YAML values. They are used only when a config value
explicitly references them, or when an adapter profile documents a host-side fallback for an
omitted provider field. Such a fallback is adapter-local, not a cross-provider convention.

Value coercion semantics:

- Path/command fields support:
  - `~` home expansion
  - `$VAR` expansion for env-backed path values
  - Apply expansion only to values intended to be local filesystem paths; do not rewrite URIs or
    arbitrary shell command strings.
- Relative `workspace.root` values resolve relative to the directory containing the selected
  `WORKFLOW.md`.

### 6.2 Dynamic Reload Semantics

Dynamic reload is REQUIRED:

- The software MUST detect `WORKFLOW.md` changes.
- On change, it MUST re-read and re-apply workflow config and prompt template without restart.
- The software MUST attempt to adjust live behavior to the new config (for example polling
  cadence, concurrency limits, active/terminal states, oh-my-pi settings, workspace paths/hooks, and
  prompt content for future runs).
- Reloaded config applies to future dispatch, retry scheduling, reconciliation decisions, hook
  execution, and agent launches.
- Implementations are not REQUIRED to restart in-flight agent sessions automatically when config
  changes.
- Extensions that manage their own listeners/resources (for example an HTTP server port change) MAY
  require restart unless the implementation explicitly supports live rebind.
- Implementations SHOULD also re-validate/reload defensively during runtime operations (for example
  before dispatch) in case filesystem watch events are missed.
- Invalid reloads MUST NOT crash the service; keep operating with the last known good effective
  configuration and emit an operator-visible error.

### 6.3 Dispatch Preflight Validation

This validation is a scheduler preflight run before attempting to dispatch new work. It validates
the workflow/config needed to poll and launch workers, not a full audit of all possible workflow
behavior.

Startup validation:

- Validate configuration before starting the scheduling loop.
- If startup validation fails, fail startup and emit an operator-visible error.

Per-tick dispatch validation:

- Re-validate before each dispatch cycle.
- If validation fails, skip dispatch for that tick, keep reconciliation active, and emit an
  operator-visible error.

Validation checks:

- Workflow file can be loaded and parsed.
- `tracker.kind` is present and supported.
- The selected adapter accepts `tracker.provider` after documented defaults and `$VAR`
  resolution.
- `omp.command` is present and non-empty.

### 6.4 Core Config Fields Summary (Cheat Sheet)

This section is intentionally redundant so a coding agent can implement the config layer quickly.
Extension fields are documented in the extension section that defines them. Core conformance does
not require recognizing or validating extension fields unless that extension is implemented.

- `tracker.kind`: string, REQUIRED; `github_projects` MUST be supported
- `tracker.provider`: object; for `github_projects`, requires `project_id` and host-side auth
- `tracker.required_labels`: list of strings, default `[]`
- `tracker.active_states`: board Status option names, default `["Todo", "In Progress"]`
- `tracker.terminal_states`: board Status option names, default `["Done"]`
- `polling.interval_ms`: integer, default `30000`
- `workspace.root`: path resolved to absolute, default `<system-temp>/symphony_workspaces`
- `hooks.after_create`: shell script or null
- `hooks.before_run`: shell script or null
- `hooks.after_run`: shell script or null
- `hooks.before_remove`: shell script or null
- `hooks.timeout_ms`: integer, default `60000`
- `agent.max_concurrent_agents`: integer, default `10`
- `agent.max_turns`: integer, default `20`
- `agent.max_retry_backoff_ms`: integer, default `300000` (5m)
- `agent.max_concurrent_agents_by_state`: map of positive integers, default `{}`
- `omp.command`: shell command string, default `omp --mode rpc --no-ui`
- `omp.turn_timeout_ms`: integer, default `3600000`
- `omp.read_timeout_ms`: integer, default `5000`
- `omp.stall_timeout_ms`: integer, default `300000`

## 7. Orchestration State Machine

The orchestrator is the only component that mutates scheduling state. All worker outcomes are
reported back to it and converted into explicit state transitions.

### 7.1 Issue Orchestration States

This is not the same as tracker states (`Todo`, `In Progress`, etc.). This is the service's internal
claim state.

1. `Unclaimed`
   - Issue is not running and has no retry scheduled.

2. `Claimed`
   - Orchestrator has reserved the issue to prevent duplicate dispatch.
   - In practice, claimed issues are either `Running` or `RetryQueued`.

3. `Running`
   - Worker task exists and the issue is tracked in `running` map.

4. `RetryQueued`
   - Worker is not running, but a retry timer exists in `retry_attempts`.

5. `Released`
   - Claim removed because issue is terminal, non-active, missing, or retry path completed without
     re-dispatch.

Important nuance:

- A successful worker exit does not mean the issue is done forever.
- The worker MAY continue through multiple back-to-back coding-agent turns before it exits.
- After each normal turn completion, the worker re-checks the tracker issue state.
- If the issue is still in an active state, the worker SHOULD start another turn on the same live
  oh-my-pi session in the same workspace, up to `agent.max_turns`.
- The first turn SHOULD use the full rendered task prompt.
- Continuation turns SHOULD send only continuation guidance to the existing session, not resend the
  original task prompt that is already present in session history.
- Once the worker exits normally, the orchestrator still schedules a short continuation retry
  (about 1 second) so it can re-check whether the issue remains active and needs another worker
  session.

### 7.2 Run Attempt Lifecycle

A run attempt transitions through these phases:

1. `PreparingWorkspace`
2. `BuildingPrompt`
3. `LaunchingAgentProcess`
4. `InitializingSession`
5. `StreamingTurn`
6. `Finishing`
7. `Succeeded`
8. `Failed`
9. `TimedOut`
10. `Stalled`
11. `CanceledByReconciliation`

Distinct terminal reasons are important because retry logic and logs differ.

### 7.3 Transition Triggers

- `Poll Tick`
  - Reconcile active runs.
  - Validate config.
  - Fetch candidate issues.
  - Dispatch until slots are exhausted.

- `Worker Exit (normal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Schedule continuation retry (attempt `1`) after the worker exhausts or finishes its in-process
    turn loop.

- `Worker Exit (abnormal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Schedule exponential-backoff retry.

- `Oh-my-pi Update Event`
  - Update live session fields, token counters, and rate limits when available.

- `Retry Timer Fired`
  - Re-fetch active candidates and attempt re-dispatch, or release claim if no longer eligible.

- `Reconciliation State Refresh`
  - Stop runs whose issue states are terminal or no longer active.

- `Stall Timeout`
  - Kill worker and schedule retry.

### 7.4 Idempotency and Recovery Rules

- The orchestrator serializes state mutations through one authority to avoid duplicate dispatch.
- `claimed` and `running` checks are REQUIRED before launching any worker.
- Reconciliation runs before dispatch on every tick.
- Restart recovery is tracker-driven and filesystem-driven (without a durable orchestrator DB).
- Startup terminal cleanup removes stale workspaces for issues already in terminal states.

## 8. Polling, Scheduling, and Reconciliation

### 8.1 Poll Loop

At startup, the service validates config, performs startup cleanup, schedules an immediate tick, and
then repeats every `polling.interval_ms`.

The effective poll interval SHOULD be updated when workflow config changes are re-applied.

Tick sequence:

1. Reconcile running issues.
2. Run dispatch preflight validation.
3. Fetch candidate issues from tracker using active states.
4. Sort issues by dispatch priority.
5. Dispatch eligible issues while slots remain.
6. Notify observability/status consumers of state changes.

If per-tick validation fails, dispatch is skipped for that tick, but reconciliation still happens
first.

### 8.2 Candidate Selection Rules

An issue is dispatch-eligible only if all are true:

- It has `id`, `identifier`, `title`, and `state`.
- Its state is in `active_states` and not in `terminal_states`.
- Its adapter-provided `dispatchable` value is `true`.
- It contains every label in `tracker.required_labels`.
- It is not already in `running`.
- It is not already in `claimed`.
- Global concurrency slots are available.
- Per-state concurrency slots are available.

For refresh and continuation checks, `issue_routable(issue)` means only that adapter-provided
`dispatchable` is true and all `tracker.required_labels` match. State, claims, and concurrency are
checked separately by the surrounding algorithm.

Sorting order (stable intent):

1. `priority` ascending for values `1..4`; all other integers and null sort after that bucket
2. `created_at` oldest first; null sorts last
3. `identifier` lexicographic tie-breaker

### 8.3 Concurrency Control

Global limit:

- `available_slots = max(max_concurrent_agents - running_count, 0)`

Per-state limit:

- `max_concurrent_agents_by_state[state]` if present (state key normalized)
- otherwise fallback to global limit

The runtime counts issues by their current tracked state in the `running` map.

### 8.4 Retry and Backoff

Retry entry creation:

- Cancel any existing retry timer for the same issue.
- Store `attempt`, `identifier`, `error`, `due_at_ms`, and new timer handle.

Backoff formula:

- Normal continuation retries after a clean worker exit use a short fixed delay of `1000` ms.
- Failure-driven retries use `delay = min(10000 * 2^(attempt - 1), agent.max_retry_backoff_ms)`.
- Power is capped by the configured max retry backoff (default `300000` / 5m).

Retry handling behavior:

1. Refresh the specific issue with `fetch_issues_by_ids([issue_id])`.
2. If not found, release claim.
3. If found in a terminal state, clean its workspace and release claim.
4. If found and still active and routable:
   - Dispatch if slots are available.
   - Otherwise requeue with error `no available orchestrator slots`.
5. If found but no longer active or routable, release claim without dispatch.

Note:

- Terminal-state workspace cleanup is handled by startup cleanup, active-run reconciliation, and
  retry refreshes that observe a terminal transition.
- ID refresh avoids treating a terminal, non-active, or newly unroutable issue as merely absent.

### 8.5 Active Run Reconciliation

Reconciliation runs every tick and has two parts.

Part A: Stall detection

- For each running issue, compute `elapsed_ms` since:
  - `last_omp_timestamp` if any event has been seen, else
  - `started_at`
- If `elapsed_ms > omp.stall_timeout_ms`, terminate the worker and queue a retry.
- If `stall_timeout_ms <= 0`, skip stall detection entirely.

Part B: Tracker state refresh

- Fetch current issue states for all running issue IDs.
- For each running issue:
  - If tracker state is terminal: terminate worker and clean workspace.
  - If tracker state is still active and routable: update the in-memory issue snapshot.
  - If tracker state is active but no longer routable: terminate worker without workspace cleanup.
  - If tracker state is neither active nor terminal: terminate worker without workspace cleanup.
- If state refresh fails, keep workers running and try again on the next tick.

### 8.6 Startup Terminal Workspace Cleanup

When the service starts:

1. Query tracker for issues in terminal states.
2. For each returned issue identifier, remove the corresponding workspace directory.
3. If the terminal-issues fetch fails, log a warning and continue startup.

This prevents stale terminal workspaces from accumulating after restarts.

## 9. Workspace Management and Safety

### 9.1 Workspace Layout

Workspace root:

- `workspace.root` (normalized absolute path)

Per-issue workspace path:

- `<workspace.root>/<workspace_key>`

Workspace persistence:

- Workspaces are reused across runs for the same issue.
- Successful runs do not auto-delete workspaces.

### 9.2 Workspace Creation and Reuse

Input: `issue.identifier`

Algorithm summary:

1. Derive `workspace_key` using Section 4.2, including the stable original-identifier hash when
   sanitization changes the identifier.
2. Compute workspace path under workspace root.
3. Ensure the workspace path exists as a directory.
4. Mark `created_now=true` only if the directory was created during this call; otherwise
   `created_now=false`.
5. If `created_now=true`, run `after_create` hook if configured.

Notes:

- This section does not assume any specific repository/VCS workflow.
- Workspace preparation beyond directory creation (for example dependency bootstrap, checkout/sync,
  code generation) is implementation-defined and is typically handled via hooks.

### 9.3 OPTIONAL Workspace Population (Implementation-Defined)

The spec does not require any built-in VCS or repository bootstrap behavior.

Implementations MAY populate or synchronize the workspace using implementation-defined logic and/or
hooks (for example `after_create` and/or `before_run`).

Failure handling:

- Workspace population/synchronization failures return an error for the current attempt.
- If failure happens while creating a brand-new workspace, implementations MAY remove the partially
  prepared directory.
- Reused workspaces SHOULD NOT be destructively reset on population failure unless that policy is
  explicitly chosen and documented.

### 9.4 Workspace Hooks

Supported hooks:

- `hooks.after_create`
- `hooks.before_run`
- `hooks.after_run`
- `hooks.before_remove`

Execution contract:

- Execute in a local shell context appropriate to the host OS, with the workspace directory as
  `cwd`.
- On POSIX systems, `sh -lc <script>` (or a stricter equivalent such as `bash -lc <script>`) is a
  conforming default.
- Hook timeout uses `hooks.timeout_ms`; default: `60000 ms`.
- Log hook start, failures, and timeouts.

Failure semantics:

- `after_create` failure or timeout is fatal to workspace creation.
- `before_run` failure or timeout is fatal to the current run attempt.
- `after_run` failure or timeout is logged and ignored.
- `before_remove` failure or timeout is logged and ignored.

### 9.5 Safety Invariants

This is the most important portability constraint.

Invariant 1: Run the coding agent only in the per-issue workspace path.

- Before launching the coding-agent subprocess, validate:
  - `cwd == workspace_path`

Invariant 2: Workspace path MUST stay inside workspace root.

- Normalize both paths to absolute.
- Require `workspace_path` to have `workspace_root` as a prefix directory.
- Reject any path outside the workspace root.

Invariant 3: Workspace key is sanitized.

- Only `[A-Za-z0-9._-]` allowed in workspace directory names.
- Replace all other characters with `_`.
- If replacement changes the identifier, append a stable original-identifier hash suffix with at
  least 64 bits of entropy so keys remain collision-resistant after sanitization.

## 10. Agent Runner Protocol (Oh-my-pi Integration)

This section defines Symphony's language-neutral responsibilities when integrating oh-my-pi.
The targeted oh-my-pi version's RPC documentation and types are the source of truth for message
payloads, transport framing, and command names:

- [RPC protocol reference](https://github.com/can1357/oh-my-pi/blob/main/docs/rpc.md)
- [Canonical RPC types](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/modes/rpc/rpc-types.ts)

Implementations MUST document the targeted oh-my-pi version and send valid commands for that
version. If this specification conflicts with the targeted protocol, that protocol controls wire
shape and transport behavior. Symphony's requirements still control scheduling, workspace
selection, prompts, continuation, and observability.

### 10.1 Launch Contract

Subprocess launch parameters:

- Command: `omp.command`
- Invocation: `bash -lc <omp.command>`
- Default: `omp --mode rpc --no-ui`
- Working directory: absolute issue workspace path
- Transport: newline-delimited JSON over stdin/stdout, not JSON-RPC 2.0
- Diagnostics: stderr, separate from the protocol stream

The client MUST wait for the `ready` frame and respect its advertised transport limits.
It SHOULD negotiate protocol v2 with `negotiate_protocol` when supported, to preserve oversized
output losslessly. A v2 client MUST validate and reassemble `rpc_chunk` frames according to the
targeted protocol, including sequence order, byte lengths, UTF-8 validity, and reassembly limits.
Inbound commands remain unchunked JSONL objects and MUST fit the advertised physical-frame limit.
Transport overflow or invalid chunk sequences MUST surface as failures, not successful truncated
results.

### 10.2 Session Startup Responsibilities

The client MUST:

- Start the RPC subprocess in the per-issue workspace with tracker secrets removed from its
  environment.
- Create a fresh session with `new_session`, or explicitly resume a session previously bound to
  this issue using a supported session command. A cancelled session transition is a startup
  failure. Never implicitly adopt another issue's history.
- Send `get_state` and extract `data.sessionId` as `omp_session_id`.
- Register implemented host tools with `set_host_tools` before sending the first prompt.
- Start the first turn with a uniquely identified `prompt` command containing the rendered issue
  prompt in `message`.
- Send later continuation prompts on the same session with continuation guidance rather than
  resending the original task.
- Apply the deployment's documented oh-my-pi CLI/configuration and host isolation policy.

The client MAY set a session name using `set_session_name`, for example
`<issue.identifier>: <issue.title>`.

Session identifiers:

- `omp_session_id` is the native oh-my-pi session ID, retained across in-worker continuations.
- `turn_id` is the host-generated `id` of a `prompt` command; oh-my-pi does not return a separate
  server-side turn ID for Symphony to use.
- Emit `session_id = "<omp_session_id>-<turn_id>"` for each Symphony turn.
- Correlate command responses and `prompt_result` frames by `id`, not arrival order.

### 10.3 Streaming Turn Processing

A Symphony turn is one submitted prompt plus all background activity it causes before the session
settles. It can span several oh-my-pi model turns and agent runs. `agent.max_turns` limits submitted
Symphony prompts, not individual `turn_end` events.

The client MUST keep reading RPC output and servicing host-tool requests while the prompt runs:

- A successful `response` to `prompt` acknowledges admission; it is not agent completion.
- A success response with `data.agentInvoked: false` completes a local-only command without a
  later `prompt_result`.
- Otherwise, wait for the matching `prompt_result`. `status: "error"` fails the turn;
  `status: "aborted"` cancels it; `status: "completed"` reports the agent's yield.
- A completed result with `sessionSettled: false` is not safe for continuation or shutdown.
  Continue consuming background runs until `session_settled` or `get_state.data.isSettled` is true.
  Errors in those background runs MUST still fail the attempt.
- Neither an arbitrary `turn_end` nor `agent_end` is sufficient proof of prompt completion or
  session quiescence.
- A failed command response, transport failure, stream silence timeout, or unexpected subprocess
  exit fails the attempt.

After successful completion and quiescence, refresh the issue and decide whether to submit another
prompt. Keep the same subprocess and session alive across in-worker continuations.

On cancellation, send `abort` when possible and terminate the process if it does not stop within
the implementation's documented shutdown deadline. On normal shutdown, close stdin and continue
draining stdout until EOF; use a bounded shutdown deadline rather than waiting indefinitely.

### 10.4 Emitted Runtime Events (Upstream to Orchestrator)

The RPC client emits structured events to the orchestrator callback. Each event SHOULD include:

- `event` (enum/string)
- `timestamp` (UTC timestamp)
- `omp_pid` (if available)
- `session_id` and `turn_id` when associated with a prompt
- OPTIONAL `usage` map (normalized token counts)
- payload fields as needed

Important normalized events include:

- `session_started`
- `startup_failed`
- `turn_completed` (only after completion and session quiescence)
- `turn_failed`
- `turn_cancelled`
- `turn_ended_with_error`
- `turn_input_required`
- `unsupported_tool_call`
- `notification`
- `other_message`
- `malformed`

Message, tool-execution, compaction, retry, and background-work updates SHOULD refresh liveness.
Token counts MUST follow Section 13.5. Rate-limit metadata is OPTIONAL; do not invent a native
rate-limit event when the targeted oh-my-pi version does not expose one.

### 10.5 Approval, Host Tools, and User Input Policy

Each implementation MUST document its approval, sandbox, and operator-confirmation posture.
Use oh-my-pi's supported CLI/configuration for tool approvals and external isolation when needed;
the RPC protocol does not supply per-thread or per-turn sandbox policy objects.

The default `--mode rpc --no-ui` is headless. Interactive tool approvals fail closed, and extension
dialogs use their headless defaults. Implementations MUST evaluate those defaults rather than
equating headless mode with automatic approval. A deployment that uses `rpc-ui` or another supported
interactive policy MUST handle `extension_ui_request` dialogs through matching
`extension_ui_response` frames or fail the run; it MUST NOT wait indefinitely for user input.
Presentation-only UI notifications do not require responses.

OPTIONAL provider-native host-tool extension:

- An adapter MAY expose provider-native tools through RPC `set_host_tools`. Each definition includes
  a unique `name`, `description`, and JSON Schema `parameters` valid for the targeted protocol.
- Tool definitions, adapter selection, and effective tracker settings MUST be bound to one session
  snapshot. Reloads apply to future sessions, not the provider/authentication of an in-flight call.
- The runtime MUST handle `host_tool_call` host-side using the active adapter configuration and
  return a `host_tool_result` with the request's `id`.
- Tool failures, invalid arguments, missing authentication, and unsupported names MUST return
  structured error content with top-level `isError: true`, without stalling the session.
- The runtime MUST honor `host_tool_cancel` for pending calls and document cancellation semantics
  for mutations that may already have reached the provider.
- The child MUST NOT need raw tracker tokens on disk or in its environment. Adapters MUST declare
  authentication-related environment names for removal from local and remote child environments.
- Pass the normalized issue, including `id` and `native_ref`, as internal tool execution context.
- Tool names, schemas, scope/authorization, mutation capability, results, errors, idempotency, and
  provider rate limits are adapter-owned and MUST be documented when tools ship.

Minimal language-neutral adapter hooks for this OPTIONAL extension:

```text
agent_tool_specs() -> list<ToolSpec>
secret_environment_names() -> list<string>
execute_agent_tool(name, arguments, context={issue}) -> ToolResult
```

`ToolResult` MUST distinguish success from failure and carry JSON-safe output translated into
the RPC result's `content` and OPTIONAL `details`. The context contains the issue, never the
credential. Literal credentials in a workspace-readable `WORKFLOW.md` defeat this isolation and
SHOULD NOT be used.

### 10.6 Timeouts and Error Mapping

Timeouts:

- `omp.read_timeout_ms`: wait for readiness and command responses during startup/sync requests,
  including prompt admission; it is not the deadline for prompt completion.
- `omp.turn_timeout_ms`: maximum silence interval while a prompt or its background activity is
  active; each RPC output resets it, so it is not a total turn runtime cap.
- `omp.stall_timeout_ms`: enforced by the orchestrator based on event inactivity.

RECOMMENDED normalized error categories:

- `omp_not_found`
- `invalid_workspace_cwd`
- `response_timeout`
- `turn_timeout`
- `port_exit`
- `response_error`
- `turn_failed`
- `turn_cancelled`
- `turn_input_required`
- `protocol_error`

### 10.7 Agent Runner Contract

The `Agent Runner` wraps workspace + prompt + oh-my-pi RPC client.

1. Create/reuse the issue workspace.
2. Build the prompt from the workflow template.
3. Start the RPC session.
4. Forward normalized RPC events to the orchestrator.
5. On any error, fail the worker attempt; the orchestrator owns retries.

Workspaces are intentionally preserved after successful runs.

## 11. Issue Tracker Integration Contract

The issue tracker boundary is deliberately small: a portable read kernel for scheduling plus
OPTIONAL provider-native agent tools. Do not add generic comment/state/attachment CRUD merely to
make providers look alike; those operations lose useful provider semantics and are not needed by
the orchestrator.

### 11.1 REQUIRED Adapter Operations

An implementation MUST support these adapter operations:

1. `fetch_issues_by_states(state_names)`
   - Return normalized issues visible in the configured tracker scope and requested state names.
   - The adapter MUST apply provider-side scope selection and pagination.
   - Used with configured active states for candidate polling and terminal states for startup
     cleanup.
   - When used for candidate polling, include active scoped issues even when
     `dispatchable=false`; the scheduler owns that final filter.
   - The orchestrator applies `required_labels`, `dispatchable`, claims, retries, and concurrency
     after normalization.
   - An empty `state_names` list MUST return an empty result without a provider request.

2. `fetch_issues_by_ids(issue_ids)`
   - Return current normalized issue snapshots for the supplied opaque dispatch IDs.
   - Used for active-run reconciliation and stale-dispatch revalidation.
   - An empty `issue_ids` list MUST return an empty result without a provider request.
   - IDs no longer visible in the configured scope are omitted; the orchestrator treats omission as
     "no longer visible" rather than inventing a synthetic state.

Both operations return either `ok(list<Issue>)` or an adapter error. For portability, an adapter
error SHOULD expose a stable category and human-readable message. An implementation MAY use a
language-native tagged error, exception, tuple, or enum instead of a literal error object when its
adapter profile documents how those public error forms map to category and message. The
orchestrator relies only on success versus failure.

The operations are atomic from the scheduler's perspective after a paging or transport failure. For
these rules, a record is malformed only when the adapter cannot produce the required normalized
fields (`id`, `identifier`, `title`, `state`, and explicit `dispatchable`) or cannot produce a
valid `Issue` after applying the optional-field fallback rules in Section 11.3. Unusable nullable
or best-effort provider metadata MAY normalize to `null`, an empty list, or omitted best-effort
entries; that fallback alone does not make a record malformed.

A state-list call MAY omit an individually malformed provider record because it was never safe to
dispatch, and SHOULD log that omission. An ID-refresh call MUST fail instead of silently omitting a
malformed requested record, because omission is meaningful. A successful
`fetch_issues_by_ids` result is complete for that call. Output order is not significant, input IDs
are treated as a set, and each dispatch ID appears at most once.

The refresh operation returns full normalized snapshots, not only state strings, because label,
assignment, routing, and provider-specific dispatchability can change while a run is active.

### 11.2 Adapter Responsibilities

Each adapter owns:

- construction from the current effective tracker configuration, including active/terminal states;
- endpoint, authentication, transport, timeouts, pagination, and rate-limit handling;
- provider-specific scope selection (project, board, team, repository, query, or equivalent);
- mapping provider payloads into the normalized Issue fields in Section 4.1.1;
- choosing a stable dispatch identity and preserving any distinct underlying IDs in `native_ref`;
- deriving `dispatchable` from provider-specific routing rules;
- preserving provider-native state names while allowing case-insensitive scheduler comparison;
- OPTIONAL provider-native agent tools and their authorization boundary.

The orchestrator MUST NOT inspect provider payloads, assume that `issue.id` is an underlying
ticket ID, or branch on provider-specific blocker, board, transition, or comment semantics.

Each adapter MUST publish a compact profile in implementation documentation, not only code,
containing:

- exact supported `tracker.kind` value;
- exact `tracker.provider` keys, defaults, secret keys/environment names, and validation errors;
- scope selection, pagination behavior, and provider request limits;
- `id` and `native_ref` mapping;
- state, label, priority, timestamp, `dispatchable`, malformed-record, and optional-field
  normalization;
- provider-native tool names/schemas, mutation capability, scope, and result/error behavior if any;
- mapping from public language-native error forms to portable transport/auth/rate-limit error
  categories and human-readable messages.

### 11.3 Normalization Rules

Adapter output MUST satisfy Section 4.1.1. In addition:

- Every listed field MUST be present in the normalized record. Nullable fields use `null`;
  collection fields use an empty list when absent.
- `id`, `identifier`, `title`, and `state` MUST be non-empty strings.
- `labels` MUST be trimmed, lowercased strings; blank labels MUST be dropped and duplicate labels
  SHOULD be removed.
- `priority` MUST be an integer or null.
- The scheduler ranks priorities `1..4` before null/unknown values; other integers sort with
  null/unknown unless an implementation documents a different mapping.
- `created_at` and `updated_at` MUST represent parsed RFC 3339 instants or null; the in-memory
  timestamp type is implementation-defined.
- Unusable provider values for nullable fields MAY normalize to `null`. Unusable best-effort
  collection entries MAY be dropped; if no usable entries remain, use an empty list. These
  fallbacks MUST NOT be used for `id`, `identifier`, `title`, `state`, or explicit
  `dispatchable`.
- Preserve provider spelling in `state`, but trim and lowercase only for scheduler comparisons.
- `blocked_by` is best-effort metadata; adapters MUST NOT invent blocker semantics they cannot
  represent reliably.
- `dispatchable` MUST be explicit. It is `true` only when provider-specific eligibility checks
  pass; the generic scheduler never tries to reconstruct those checks from `native_ref`.
- `native_ref` MUST be null or a JSON-safe object containing only non-secret values safe to expose
  in prompt/tool context. If provider metadata cannot be represented safely, normalize
  `native_ref` to null; otherwise preserve the retained object verbatim.

### 11.4 Error Handling Contract

RECOMMENDED adapter error categories:

- `unsupported_tracker_kind`
- `invalid_tracker_config`
- `missing_tracker_secret`
- `tracker_request` (transport failure)
- `tracker_status` (non-success response)
- `tracker_response` (malformed or semantically invalid payload)
- `tracker_pagination` (pagination integrity failure)
- `tracker_rate_limited`

For portability, every adapter profile MUST document how each public language-native error form
maps to a stable `category` and human-readable `message`. A literal `{category, message}`
object is not required. Adapters MAY add `retryable`, `retry_after_ms`, provider status, and
provider-specific detail, but the orchestrator only relies on success vs. failure.

Orchestrator behavior on tracker errors:

- Candidate fetch failure: log and skip dispatch for this tick.
- Running-state refresh failure: log and keep active workers running.
- Startup terminal cleanup failure: log warning and continue startup.

### 11.5 Tracker Writes and Agent Tools (Important Boundary)

Symphony does not require first-class tracker write APIs in the orchestrator.

- Ticket mutations (state transitions, comments, attachments, PR metadata) are typically handled by
  the coding agent through the selected adapter's provider-native tools.
- Tools execute in Symphony with the configured adapter credential; the child receives tool results,
  not a raw token.
- The current normalized issue is available to tool execution as context, including opaque
  `native_ref`, so adapters can retain provider richness without adding it to the core scheduler.
- The service remains a scheduler/runner and tracker reader.
- Workflow-specific success often means "reached the next handoff state" (for example
  `Human Review`) rather than tracker terminal state `Done`.

### 11.6 GitHub Projects Board Profile (REQUIRED)

The required adapter is `tracker.kind: github_projects`. A board is a view of a GitHub Projects
`ProjectV2` project; Symphony schedules from project items and their Status values, not from a
particular view's visual layout or filters. Projects (classic) are not part of this profile.

API reference:
[Using the API to manage Projects](https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/using-the-api-to-manage-projects).

#### 11.6.1 Provider Configuration

`tracker.provider` keys:

- `project_id` (non-empty string, REQUIRED): GraphQL node ID of the `ProjectV2` project, not its
  numeric URL project number. Resolve it from `organization(login).projectV2(number).id` or
  `user(login).projectV2(number).id` during setup.
- `endpoint` (HTTPS URL): default `https://api.github.com/graphql`; deployments MAY configure the
  corresponding GitHub Enterprise GraphQL endpoint.
- `api_key` (secret string or `$VAR_NAME`): host-side GitHub credential. If omitted, resolve
  `GH_TOKEN`, then `GITHUB_TOKEN`, taking the first non-empty value. An explicit empty or unresolved
  value is a missing secret, not permission to fall back.
- `status_field` (non-empty string): default `Status`; identifies one project single-select field
  by its exact name. A missing, ambiguous, or non-single-select field is a configuration error.

Default active states are `["Todo", "In Progress"]`; default terminal states are `["Done"]`.
Workflows using other board columns MUST configure their option names explicitly. Validate that
every configured state matches an option in the selected field after scheduler normalization;
do not translate unknown states into issue `OPEN` or `CLOSED` values.

Authentication MUST authorize reading the project and the issues in its repositories. Classic
personal access tokens need `read:project` for reads or `project` for mutations, plus applicable
repository access for private issues. Fine-grained tokens and GitHub Apps MUST have corresponding
Projects and repository permissions for the operations they perform. Project access alone does
not imply access to private issue content.

Declare `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`, and any
environment name referenced by `api_key` as tracker-secret environment names. Keep this credential
host-side and distinct from any model-provider authentication required by oh-my-pi.

Example `WORKFLOW.md`:

```yaml
---
tracker:
  kind: github_projects
  provider:
    project_id: $GITHUB_PROJECT_ID
    api_key: $GITHUB_TOKEN
    status_field: Status
  active_states: [Todo, In Progress]
  terminal_states: [Done]
polling:
  interval_ms: 30000
omp:
  command: omp --mode rpc --no-ui
---
Work on GitHub issue {{ issue.identifier }}: {{ issue.title }}.
{{ issue.description }}
```

`project_id` also supports `$VAR_NAME` resolution; the example requires `GITHUB_PROJECT_ID` to
contain a real project node ID and `GITHUB_TOKEN` to contain an authorized host-side credential.

#### 11.6.2 Reads, Scope, and Normalization

- Fetch items through `node(id: project_id) { ... on ProjectV2 { items(...) } }`.
  Paginate using `pageInfo.hasNextPage` and `endCursor`; use at most 100 nodes per connection page.
  Fully paginate any required nested connections, including labels and field metadata/values,
  rather than silently using only the first page.
- `fetch_issues_by_states` scans the configured project's items and filters by the selected Status
  field locally. GitHub's project-items connection is not assumed to support a state predicate.
- `fetch_issues_by_ids` resolves `ProjectV2Item` node IDs in bounded batches, verifies project
  membership, and returns full snapshots, including current Status, labels, and eligibility.
- Only items whose accessible `content` is a GitHub `Issue` are schedulable in this profile.
  Pull requests, draft items, and inaccessible/deleted content are outside its scope and omitted.
  Removed items and items belonging to another project are omitted on refresh.
- Normalize `id` from the `ProjectV2Item.id`, not `Issue.id`.
- Normalize `native_ref` as `{project_id, project_item_id, issue_id, repository, issue_number}`,
  where `repository` is `owner/name`; these are non-secret references for provider-native tools.
- Normalize `identifier` as `<owner>/<repository>#<number>` to disambiguate issues across
  repositories. Workspace naming still follows Section 4.2 sanitization and hash-suffix rules.
- Map `title`, `description`, `url`, `created_at`, and `updated_at` from the issue's title, body,
  URL, and timestamps. Map labels through Section 11.3. Choose the lexicographically smallest
  assignee node ID for `assignee_id`, or null when unassigned; this does not impose an assignee filter.
- Map `state` from the configured project's single-select Status option name. Missing Status is
  a malformed required field: omit/log it in state-list reads and fail a requested ID refresh,
  as specified in Section 11.1.
- Set `priority` and `branch_name` to null and `blocked_by` to `[]`; this profile does not infer
  priorities, branches, or blocker relationships from labels or text.
- Set `dispatchable=true` only for unarchived items with an open, unlocked issue in the configured
  project. Otherwise return a valid issue with `dispatchable=false` when its content and Status
  remain readable. State-list reads include readable archived/closed terminal items so startup
  cleanup can still find their workspaces.
- Closing an issue does not imply that its board Status becomes `Done`. Board Status remains the
  workflow state; issue closure only prevents dispatch/continuation.
- Treat GraphQL `errors` as failures even with HTTP 200; never hand a partial page/result to the
  scheduler. Apply the atomic pagination/error rules in Section 11.1.

Map invalid project/field/state configuration to `invalid_tracker_config`, missing credentials to
`missing_tracker_secret`, transport failures to `tracker_request`, non-success HTTP responses to
`tracker_status`, malformed GraphQL payloads or non-rate-limit GraphQL errors to `tracker_response`,
invalid/repeated cursors to `tracker_pagination`, and provider-identified rate limiting to
`tracker_rate_limited`. Preserve a human-readable message without exposing the credential.

#### 11.6.3 Board Updates Through Agent Tools

When provider-native host tools are shipped, board transitions use
`updateProjectV2ItemFieldValue` with the configured `projectId`, `itemId`, Status `fieldId`, and
`value.singleSelectOptionId`. Resolve the option ID from the intended Status name; never treat an
option's display name as its ID. Issue comments and issue closure operate on the underlying issue
ID instead. A board transition and an issue close/reopen are distinct operations.

The host MUST verify that a tool's target belongs to the configured project before reading or
mutating it. Advertise only the adapter's documented operations and authorize them with the
host-side credential through the RPC host-tool mechanism in Section 10.5.

## 12. Prompt Construction and Context Assembly

### 12.1 Inputs

Inputs to prompt rendering:

- `workflow.prompt_template`
- normalized `issue` object
- OPTIONAL `attempt` integer (retry/continuation metadata)

### 12.2 Rendering Rules

- Render with strict variable checking.
- Render with strict filter checking.
- Convert issue object keys to strings for template compatibility.
- Preserve nested arrays/maps (labels, blockers) so templates can iterate.

### 12.3 Retry/Continuation Semantics

`attempt` SHOULD be passed to the template as a 1-based retry/continuation count:

- first run: `attempt` is null or absent;
- any later run: `attempt` is an integer.

The core `attempt` value does not distinguish a normal continuation from an error/timeout/stall
retry. An implementation MAY expose an additional `retry_kind` template field if workflows need
that distinction, but it is not part of core conformance.

### 12.4 Failure Semantics

If prompt rendering fails:

- Fail the run attempt immediately.
- Let the orchestrator treat it like any other worker failure and decide retry behavior.

## 13. Logging, Status, and Observability

### 13.1 Logging Conventions

REQUIRED context fields for issue-related logs:

- `issue_id`
- `issue_identifier`

REQUIRED context for coding-agent session lifecycle logs:

- `session_id`

Message formatting requirements:

- Use stable `key=value` phrasing.
- Include action outcome (`completed`, `failed`, `retrying`, etc.).
- Include concise failure reason when present.
- Avoid logging large raw payloads unless necessary.

### 13.2 Logging Outputs and Sinks

The spec does not prescribe where logs are written (stderr, file, remote sink, etc.).

Requirements:

- Operators MUST be able to see startup/validation/dispatch failures without attaching a debugger.
- Implementations MAY write to one or more sinks.
- If a configured log sink fails, the service SHOULD continue running when possible and emit an
  operator-visible warning through any remaining sink.

### 13.3 Runtime Snapshot / Monitoring Interface (OPTIONAL but RECOMMENDED)

If the implementation exposes a synchronous runtime snapshot (for dashboards or monitoring), it
SHOULD return:

- `running` (list of running session rows)
- each running row SHOULD include `turn_count`
- `retrying` (list of retry queue rows)
- session and retry rows SHOULD include the tracker-provided issue URL when available
- `omp_totals`
  - `input_tokens`
  - `output_tokens`
  - `total_tokens`
  - `seconds_running` (aggregate runtime seconds as of snapshot time, including active sessions)
- `rate_limits` (latest coding-agent rate limit payload, if available)

RECOMMENDED snapshot error modes:

- `timeout`
- `unavailable`

### 13.4 OPTIONAL Human-Readable Status Surface

A human-readable status surface (terminal output, dashboard, etc.) is OPTIONAL and
implementation-defined.

If present, it SHOULD draw from orchestrator state/metrics only and MUST NOT be REQUIRED for
correctness.

### 13.5 Session Metrics and Token Accounting

Token accounting rules:

- Use cumulative `get_session_stats` responses from the targeted oh-my-pi version as the
  authoritative session totals. Refresh them after each settled Symphony turn and before normal
  shutdown; implementations MAY also refresh during streaming for live status.
- Normalize `data.tokens.input`, `data.tokens.output`, and `data.tokens.total` into
  `input_tokens`, `output_tokens`, and `total_tokens`. Cache/reasoning breakdowns MAY be exposed
  separately; do not assume `total_tokens` equals input plus output for every provider.
- Track deltas relative to the last reported totals for the same `omp_session_id` to avoid
  double-counting repeated snapshots. If resuming a session, record its starting totals as a
  baseline so earlier attempts are not counted again.
- Streaming `message_update`, `message_end`, and `agent_end` can repeat the same usage. They MUST
  NOT be added again on top of the authoritative cumulative statistics.
- If live counts are derived from messages, deduplicate them by stable message identity and
  reconcile them with session statistics rather than treating every frame as new consumption.
- Reset session baselines when the native session ID changes and accumulate only new consumption
  in orchestrator state.

Runtime accounting:

- Runtime SHOULD be reported as a live aggregate at snapshot/render time.
- Implementations MAY maintain a cumulative counter for ended sessions and add active-session
  elapsed time derived from `running` entries (for example `started_at`) when producing a
  snapshot/status view.
- Add run duration seconds to the cumulative ended-session runtime when a session ends (normal exit
  or cancellation/termination).
- Continuous background ticking of runtime totals is not REQUIRED.

Rate-limit tracking:

- Track the latest provider rate-limit payload only when the targeted integration exposes it.
- Use `null` when unavailable; oh-my-pi RPC does not guarantee a rate-limit snapshot event.
- Any human-readable presentation of rate-limit data is implementation-defined.

### 13.6 Humanized Agent Event Summaries (OPTIONAL)

Humanized summaries of raw agent protocol events are OPTIONAL.

If implemented:

- Treat them as observability-only output.
- Do not make orchestrator logic depend on humanized strings.

### 13.7 OPTIONAL HTTP Server Extension

This section defines an OPTIONAL HTTP interface for observability and operational control.

If implemented:

- The HTTP server is an extension and is not REQUIRED for conformance.
- The implementation MAY serve server-rendered HTML or a client-side application for the dashboard.
- The dashboard/API MUST be observability/control surfaces only and MUST NOT become REQUIRED for
  orchestrator correctness.

Extension config:

- `server.port` (integer, OPTIONAL)
  - Enables the HTTP server extension.
  - `0` requests an ephemeral port for local development and tests.
  - CLI `--port` overrides `server.port` when both are present.

Enablement (extension):

- Start the HTTP server when a CLI `--port` argument is provided.
- Start the HTTP server when `server.port` is present in `WORKFLOW.md` front matter.
- The `server` top-level key is owned by this extension.
- Positive `server.port` values bind that port.
- Implementations SHOULD bind loopback by default (`127.0.0.1` or host equivalent) unless explicitly
  configured otherwise.
- Changes to HTTP listener settings (for example `server.port`) do not need to hot-rebind;
  restart-required behavior is conformant.

#### 13.7.1 Human-Readable Dashboard (`/`)

- Host a human-readable dashboard at `/`.
- The returned document SHOULD depict the current state of the system (for example active sessions,
  retry delays, token consumption, runtime totals, recent events, and health/error indicators).
- It is up to the implementation whether this is server-generated HTML or a client-side app that
  consumes the JSON API below.

#### 13.7.2 JSON REST API (`/api/v1/*`)

Provide a JSON REST API under `/api/v1/*` for current runtime state and operational debugging.

Minimum endpoints:

- `GET /api/v1/state`
  - Returns a summary view of the current system state (running sessions, retry queue/delays,
    aggregate token/runtime totals, latest rate limits, and any additional tracked summary fields).
  - Suggested response shape:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "counts": {
        "running": 2,
        "retrying": 1
      },
      "running": [
        {
          "issue_id": "PVTI_example649",
          "issue_identifier": "octo-org/example#649",
          "issue_url": "https://github.com/octo-org/example/issues/649",
          "state": "In Progress",
          "session_id": "omp-session-1-prompt-7",
          "turn_count": 7,
          "last_event": "turn_completed",
          "last_message": "",
          "started_at": "2026-02-24T20:10:12Z",
          "last_event_at": "2026-02-24T20:14:59Z",
          "tokens": {
            "input_tokens": 1200,
            "output_tokens": 800,
            "total_tokens": 2000
          }
        }
      ],
      "retrying": [
        {
          "issue_id": "PVTI_example650",
          "issue_identifier": "octo-org/example#650",
          "issue_url": "https://github.com/octo-org/example/issues/650",
          "attempt": 3,
          "due_at": "2026-02-24T20:16:00Z",
          "error": "no available orchestrator slots"
        }
      ],
      "omp_totals": {
        "input_tokens": 5000,
        "output_tokens": 2400,
        "total_tokens": 7400,
        "seconds_running": 1834.2
      },
      "rate_limits": null
    }
    ```

- `GET /api/v1/<issue_identifier>`
  - Returns issue-specific runtime/debug details for the identified issue, including any information
    the implementation tracks that is useful for debugging.
  - Percent-encode the entire identifier as one path parameter. For example,
    `octo-org/example#649` uses `/api/v1/octo-org%2Fexample%23649`; the server MUST recover the
    original identifier without interpreting its encoded slash as another route segment.
  - Suggested response shape:

    ```json
    {
      "issue_identifier": "octo-org/example#649",
      "issue_id": "PVTI_example649",
      "status": "running",
      "workspace": {
        "path": "/tmp/symphony_workspaces/octo-org_example_649_25205aad6040fe60"
      },
      "attempts": {
        "restart_count": 1,
        "current_retry_attempt": 2
      },
      "running": {
        "session_id": "omp-session-1-prompt-7",
        "turn_count": 7,
        "state": "In Progress",
        "started_at": "2026-02-24T20:10:12Z",
        "last_event": "notification",
        "last_message": "Working on tests",
        "last_event_at": "2026-02-24T20:14:59Z",
        "tokens": {
          "input_tokens": 1200,
          "output_tokens": 800,
          "total_tokens": 2000
        }
      },
      "retry": null,
      "logs": {
        "omp_session_logs": [
          {
            "label": "latest",
            "path": "/var/log/symphony/omp/octo-org_example_649_25205aad6040fe60/latest.log",
            "url": null
          }
        ]
      },
      "recent_events": [
        {
          "at": "2026-02-24T20:14:59Z",
          "event": "notification",
          "message": "Working on tests"
        }
      ],
      "last_error": null,
      "tracked": {}
    }
    ```

  - If the issue is unknown to the current in-memory state, return `404` with an error response (for
    example `{\"error\":{\"code\":\"issue_not_found\",\"message\":\"...\"}}`).

- `POST /api/v1/refresh`
  - Queues an immediate tracker poll + reconciliation cycle (best-effort trigger; implementations
    MAY coalesce repeated requests).
  - Suggested request body: empty body or `{}`.
  - Suggested response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "coalesced": false,
      "requested_at": "2026-02-24T20:15:30Z",
      "operations": ["poll", "reconcile"]
    }
    ```

API design notes:

- The JSON shapes above are the RECOMMENDED baseline for interoperability and debugging ergonomics.
- Implementations MAY add fields, but SHOULD avoid breaking existing fields within a version.
- Endpoints SHOULD be read-only except for operational triggers like `/refresh`.
- Unsupported methods on defined routes SHOULD return `405 Method Not Allowed`.
- API errors SHOULD use a JSON envelope such as `{"error":{"code":"...","message":"..."}}`.
- If the dashboard is a client-side app, it SHOULD consume this API rather than duplicating state
  logic.

## 14. Failure Model and Recovery Strategy

### 14.1 Failure Classes

1. `Workflow/Config Failures`
   - Missing `WORKFLOW.md`
   - Invalid YAML front matter
   - Unsupported tracker kind or invalid adapter-owned tracker configuration
   - Missing coding-agent executable

2. `Workspace Failures`
   - Workspace directory creation failure
   - Workspace population/synchronization failure (implementation-defined; can come from hooks)
   - Invalid workspace path configuration
   - Hook timeout/failure

3. `Agent Session Failures`
   - Startup handshake failure
   - Turn failed/cancelled
   - Turn timeout
   - User input requested and handled as failure by the implementation's documented policy
   - Subprocess exit
   - Stalled session (no activity)

4. `Tracker Failures`
   - Provider transport errors
   - Non-success provider responses
   - Provider-reported application errors
   - Malformed payloads

5. `Observability Failures`
   - Snapshot timeout
   - Dashboard render errors
   - Log sink configuration failure

### 14.2 Recovery Behavior

- Dispatch validation failures:
  - Skip new dispatches.
  - Keep service alive.
  - Continue reconciliation where possible.

- Worker failures:
  - Convert to retries with exponential backoff.

- Tracker candidate-fetch failures:
  - Skip this tick.
  - Try again on next tick.

- Reconciliation state-refresh failures:
  - Keep current workers.
  - Retry on next tick.

- Dashboard/log failures:
  - Do not crash the orchestrator.

### 14.3 Partial State Recovery (Restart)

Current design is intentionally in-memory for scheduler state.
Restart recovery means the service can resume useful operation by polling tracker state and reusing
preserved workspaces. It does not mean retry timers, running sessions, or live worker state survive
process restart.

After restart:

- No retry timers are restored from prior process memory.
- No running sessions are assumed recoverable.
- Service recovers by:
  - startup terminal workspace cleanup
  - fresh polling of active issues
  - re-dispatching eligible work

### 14.4 Operator Intervention Points

Operators can control behavior by:

- Editing `WORKFLOW.md` (prompt and most runtime settings).
- `WORKFLOW.md` changes are detected and re-applied automatically without restart according to
  Section 6.2.
- Changing issue states in the tracker:
  - terminal state -> running session is stopped and workspace cleaned when reconciled
  - non-active state -> running session is stopped without cleanup
- Restarting the service for process recovery or deployment (not as the normal path for applying
  workflow config changes).

## 15. Security and Operational Safety

### 15.1 Trust Boundary Assumption

Each implementation defines its own trust boundary.

Operational safety requirements:

- Implementations SHOULD state clearly whether they are intended for trusted environments, more
  restrictive environments, or both.
- Implementations SHOULD state clearly whether they rely on auto-approved actions, operator
  approvals, stricter sandboxing, or some combination of those controls.
- Workspace isolation and path validation are important baseline controls, but they are not a
  substitute for whatever approval and sandbox policy an implementation chooses.

### 15.2 Filesystem Safety Requirements

Mandatory:

- Workspace path MUST remain under configured workspace root.
- Coding-agent cwd MUST be the per-issue workspace path for the current run.
- Workspace directory names MUST use sanitized identifiers.

RECOMMENDED additional hardening for ports:

- Run under a dedicated OS user.
- Restrict workspace root permissions.
- Mount workspace root on a dedicated volume if possible.

### 15.3 Secret Handling

- Support `$VAR` indirection in workflow config.
- Do not log API tokens or secret env values.
- Validate presence of secrets without printing them.
- Execute provider-native tracker tools in the Symphony host process with the configured adapter
  credential.
- Do not pass tracker credentials through the coding-agent child environment. Adapters MUST declare
  secret environment names so local and remote launchers can remove them from child environments.
- Do not place literal tracker credentials in a repo-owned `WORKFLOW.md` when the child can read
  that workspace; use host-side secret references instead.

### 15.4 Hook Script Safety

Workspace hooks are arbitrary shell scripts from `WORKFLOW.md`.

Implications:

- Hooks are fully trusted configuration.
- Hooks run inside the workspace directory.
- Hook output SHOULD be truncated in logs.
- Hook timeouts are REQUIRED to avoid hanging the orchestrator.

### 15.5 Harness Hardening Guidance

Running oh-my-pi agents against repositories, issue trackers, and other inputs that can contain
sensitive data or externally-controlled content can be dangerous. A permissive deployment can lead
to data leaks, destructive mutations, or full machine compromise if the agent is induced to execute
harmful commands or use overly-powerful integrations.

Implementations SHOULD explicitly evaluate their own risk profile and harden the execution harness
where appropriate. This specification intentionally does not mandate a single hardening posture, but
implementations SHOULD NOT assume that tracker data, repository contents, prompt inputs, or tool
arguments are fully trustworthy just because they originate inside a normal workflow.

Possible hardening measures include:

- Restricting oh-my-pi's enabled tools and configuring its supported approval controls instead of
  enabling unrestricted automatic approval.
- Adding OS/container/VM sandboxing, network restrictions, or separate credentials; headless RPC
  mode and workspace cwd selection do not provide an OS security boundary.
- Filtering which issues, projects, boards, teams, labels, or other tracker sources are eligible
  for dispatch so untrusted or out-of-scope tasks do not automatically reach the agent.
- Narrowing provider-native tools so they can only read or mutate data inside the intended tracker
  scope, rather than exposing general workspace-wide tracker access.
- Reducing the set of client-side tools, credentials, filesystem paths, and network destinations
  available to the agent to the minimum needed for the workflow.

The correct controls are deployment-specific, but implementations SHOULD document them clearly and
treat harness hardening as part of the core safety model rather than an optional afterthought.

## 16. Reference Algorithms (Language-Agnostic)

### 16.1 Service Startup

```text
function start_service():
  configure_logging()
  start_observability_outputs()
  start_workflow_watch(on_change=reload_and_reapply_workflow)

  state = {
    poll_interval_ms: get_config_poll_interval_ms(),
    max_concurrent_agents: get_config_max_concurrent_agents(),
    running: {},
    claimed: set(),
    retry_attempts: {},
    completed: set(),
    omp_totals: {input_tokens: 0, output_tokens: 0, total_tokens: 0, seconds_running: 0},
    omp_rate_limits: null
  }

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    fail_startup(validation)

  startup_terminal_workspace_cleanup()
  schedule_tick(delay_ms=0)

  event_loop(state)
```

### 16.2 Poll-and-Dispatch Tick

```text
on_tick(state):
  state = reconcile_running_issues(state)

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  issues = tracker.fetch_issues_by_states(active_states)
  if issues failed:
    log_tracker_error()
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  for issue in sort_for_dispatch(issues):
    if no_available_slots(state):
      break

    if should_dispatch(issue, state):
      state = dispatch_issue(issue, state, attempt=null)

  notify_observers()
  schedule_tick(state.poll_interval_ms)
  return state
```

### 16.3 Reconcile Active Runs

```text
function reconcile_running_issues(state):
  state = reconcile_stalled_runs(state)

  running_ids = keys(state.running)
  if running_ids is empty:
    return state

  refreshed = tracker.fetch_issues_by_ids(running_ids)
  if refreshed failed:
    log_debug("keep workers running")
    return state

  for issue in refreshed:
    if issue.state in terminal_states:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=true)
    else if issue.state in active_states and issue_routable(issue):
      state.running[issue.id].issue = issue
    else:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=false)

  returned_ids = set(issue.id for issue in refreshed)
  for missing_id in running_ids - returned_ids:
    state = terminate_running_issue(state, missing_id, cleanup_workspace=false)

  return state
```

### 16.4 Dispatch One Issue

```text
function dispatch_issue(issue, state, attempt):
  worker = spawn_worker(
    fn -> run_agent_attempt(issue, attempt, parent_orchestrator_pid) end
  )

  if worker spawn failed:
    return schedule_retry(state, issue.id, next_attempt(attempt), {
      identifier: issue.identifier,
      error: "failed to spawn agent"
    })

  state.running[issue.id] = {
    worker_handle,
    monitor_handle,
    identifier: issue.identifier,
    issue,
    session_id: null,
    omp_pid: null,
    last_omp_message: null,
    last_omp_event: null,
    last_omp_timestamp: null,
    omp_input_tokens: 0,
    omp_output_tokens: 0,
    omp_total_tokens: 0,
    last_reported_input_tokens: 0,
    last_reported_output_tokens: 0,
    last_reported_total_tokens: 0,
    retry_attempt: normalize_attempt(attempt),
    started_at: now_utc()
  }

  state.claimed.add(issue.id)
  state.retry_attempts.remove(issue.id)
  return state
```

### 16.5 Worker Attempt (Workspace + Prompt + Agent)

```text
function run_agent_attempt(issue, attempt, orchestrator_channel):
  workspace = workspace_manager.create_for_issue(issue.identifier)
  if workspace failed:
    fail_worker("workspace error")

  if run_hook("before_run", workspace.path) failed:
    fail_worker("before_run hook error")

  session = omp_rpc.start_session(workspace=workspace.path)
  if session failed:
    run_hook_best_effort("after_run", workspace.path)
    fail_worker("agent session startup error")

  max_turns = config.agent.max_turns
  turn_number = 1

  while true:
    prompt = build_turn_prompt(workflow_template, issue, attempt, turn_number, max_turns)
    if prompt failed:
      omp_rpc.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("prompt error")

    turn_result = omp_rpc.run_turn(
      session=session,
      prompt=prompt,
      issue=issue,
      on_message=(msg) -> send(orchestrator_channel, {omp_update, issue.id, msg})
    )

    if turn_result failed:
      omp_rpc.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("agent turn error")

    # run_turn returns success only after prompt completion, session quiescence,
    # and the session-statistics refresh described in Sections 10.3 and 13.5.

    refreshed_issue = tracker.fetch_issues_by_ids([issue.id])
    if refreshed_issue failed:
      omp_rpc.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("issue state refresh error")

    if refreshed_issue is empty:
      break

    issue = refreshed_issue[0]

    if issue.state is not active or not issue_routable(issue):
      break

    if turn_number >= max_turns:
      break

    turn_number = turn_number + 1

  omp_rpc.stop_session(session)
  run_hook_best_effort("after_run", workspace.path)

  exit_normal()
```

### 16.6 Worker Exit and Retry Handling

```text
on_worker_exit(issue_id, reason, state):
  running_entry = state.running.remove(issue_id)
  state = add_runtime_seconds_to_totals(state, running_entry)

  if reason == normal:
    state.completed.add(issue_id)  # bookkeeping only
    state = schedule_retry(state, issue_id, 1, {
      identifier: running_entry.identifier,
      delay_type: continuation
    })
  else:
    state = schedule_retry(state, issue_id, next_attempt_from(running_entry), {
      identifier: running_entry.identifier,
      error: format("worker exited: %reason")
    })

  notify_observers()
  return state
```

```text
on_retry_timer(issue_id, state):
  retry_entry = state.retry_attempts.pop(issue_id)
  if missing:
    return state

  refreshed = tracker.fetch_issues_by_ids([issue_id])
  if fetch failed:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: retry_entry.identifier,
      error: "retry refresh failed"
    })

  issue = find_by_id(refreshed, issue_id)
  if issue is null:
    state.claimed.remove(issue_id)
    return state

  if not retry_dispatch_allowed(issue, state, ignore_existing_claim=issue_id):
    state.claimed.remove(issue_id)
    return state

  if no_available_slots(state):
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: issue.identifier,
      error: "no available orchestrator slots"
    })

  return dispatch_issue(issue, state, attempt=retry_entry.attempt)
```

## 17. Test and Validation Matrix

A conforming implementation SHOULD include tests that cover the behaviors defined in this
specification.

Validation profiles:

- `Core Conformance`: deterministic tests REQUIRED for all conforming implementations.
- `Extension Conformance`: REQUIRED only for OPTIONAL features that an implementation chooses to
  ship.
- `Real Integration Profile`: environment-dependent smoke/integration checks RECOMMENDED before
  production use.

Unless otherwise noted, Sections 17.1 through 17.7 are `Core Conformance`. Bullets that begin with
`If ... is implemented` are `Extension Conformance`.

### 17.1 Workflow and Config Parsing

- Workflow file path precedence:
  - explicit runtime path is used when provided
  - cwd default is `WORKFLOW.md` when no explicit runtime path is provided
- Workflow file changes are detected and trigger re-read/re-apply without restart
- Invalid workflow reload keeps last known good effective configuration and emits an
  operator-visible error
- Missing `WORKFLOW.md` returns typed error
- Invalid YAML front matter returns typed error
- Front matter non-map returns typed error
- Config defaults apply when OPTIONAL values are missing
- `tracker.kind` validation enforces an implementation-supported adapter
- `tracker.provider` preserves adapter-owned keys and validates them through the selected adapter
- `$VAR` resolution works for documented adapter secret keys and path values
- `~` path expansion works
- `omp.command` is preserved as a shell command string
- Per-state concurrency override map normalizes state names and ignores invalid values
- Prompt template renders `issue` and `attempt`
- Prompt rendering fails on unknown variables (strict mode)

### 17.2 Workspace Manager and Safety

- Deterministic workspace path per issue identifier
- Missing workspace directory is created
- Existing workspace directory is reused
- Existing non-directory path at workspace location is handled safely (replace or fail per
  implementation policy)
- OPTIONAL workspace population/synchronization errors are surfaced
- `after_create` hook runs only on new workspace creation
- `before_run` hook runs before each attempt and failure/timeouts abort the current attempt
- `after_run` hook runs after each attempt and failure/timeouts are logged and ignored
- `before_remove` hook runs on cleanup and failures/timeouts are ignored
- Workspace path sanitization, stable original-identifier-hash collision resistance, and root
  containment invariants are enforced before agent launch
- Identifiers unchanged by sanitization keep their deterministic workspace key; conformance tests
  include distinct identifiers that sanitize to the same text and verify distinct keys
- Agent launch uses the per-issue workspace path as cwd and rejects out-of-root paths

### 17.3 Issue Tracker Adapter

- Candidate issue fetch applies configured active states and adapter-owned scope selection
- Empty `fetch_issues_by_states([])` returns empty without a provider call
- Empty `fetch_issues_by_ids([])` returns empty without a provider call
- Pagination preserves order across multiple pages
- Labels are normalized to lowercase
- Unusable optional provider metadata normalizes to null/empty without hiding valid required fields
- State-list reads log omitted malformed required records; ID refresh fails malformed requested
  records instead of treating them as invisible
- Refresh by opaque dispatch ID returns full normalized issue snapshots
- A distinct provider ticket ID or project-item ID is preserved in `native_ref` when needed
- Provider-specific routing/blocker/assignment rules become explicit `dispatchable`
- The adapter publishes the required compact profile for config, scope, normalization, tools, and
  portable error mapping
- Error mapping covers config, request, non-success response, malformed payload, pagination, and
  rate limiting, including documented category/message mappings for language-native errors
- GitHub Projects config resolves project node ID/auth and validates the single-select Status field
- Board Status, not issue `OPEN`/`CLOSED`, drives active/terminal workflow states
- Multi-repository identifiers remain unique; project-item IDs retain distinct underlying issue IDs
- PR/draft/inaccessible items are excluded, while archived/closed issues cannot dispatch
- Missing Status follows malformed-record rules rather than inventing a state
- Refresh rejects out-of-project items; pagination and GraphQL errors never produce partial success

### 17.4 Orchestrator Dispatch, Reconciliation, and Retry

- Dispatch sort order is priority then oldest creation time
- `dispatchable=false` issues are not eligible
- Required-label filtering is case-insensitive and applies after adapter normalization
- Active-state issue refresh updates running entry state
- Non-active state stops running agent without workspace cleanup
- Terminal state stops running agent and cleans workspace
- Reconciliation with no running issues is a no-op
- Normal worker exit schedules a short continuation retry (attempt 1)
- Abnormal worker exit increments retries with 10s-based exponential backoff
- Retry backoff cap uses configured `agent.max_retry_backoff_ms`
- Retry queue entries include attempt, due time, identifier, and error
- Stall detection kills stalled sessions and schedules retry
- Slot exhaustion requeues retries with explicit error reason
- If a snapshot API is implemented, it returns running rows, retry rows, token totals, and rate
  limits
- If a snapshot API is implemented, timeout/unavailable cases are surfaced

### 17.5 Oh-my-pi RPC Client

- Launch uses workspace cwd and `bash -lc <omp.command>` without tracker secrets in the child env
- Startup waits for `ready`, respects advertised frame limits, and negotiates supported v2 framing
- Chunk reassembly rejects invalid order, lengths, UTF-8, and interrupted sequences
- Fresh/resumed sessions remain bound to the intended issue; cancelled transitions fail startup
- `get_state.data.sessionId` and host-generated prompt IDs produce correct session/turn metadata
- Responses and prompt results are correlated by request ID despite interleaved events
- A prompt acknowledgement does not complete a turn
- Local-only `data.agentInvoked: false` completion does not wait for a nonexistent prompt result
- `prompt_result.status` distinguishes success, error, and abort
- A completed prompt with pending async work waits for session quiescence; background failures
  still fail the attempt
- Continuations reuse the same process/session and do not resend the original prompt
- Request-response, stream-silence, and orchestrator stall timeouts are enforced separately
- Protocol stdout and diagnostic stderr are handled separately
- Headless approval defaults and interactive UI requests follow the documented policy without
  stalling; headless mode is not assumed to approve everything
- Cancellation aborts active work; bounded shutdown drains stdout after stdin closes
- Usage extraction follows cumulative session statistics; unavailable rate limits remain null
- If client-side host tools are implemented, startup registers them using `set_host_tools`
- If client-side host tools are implemented, `host_tool_call` results/errors preserve request IDs
  and `host_tool_cancel` cancels pending execution
- If provider-native agent tools are implemented:
  - only the selected adapter's tools are advertised to the session
  - valid inputs execute host-side with configured adapter auth
  - the current normalized issue and `native_ref` are available as internal tool context
  - tracker secrets are not inherited by the coding-agent child process
  - invalid arguments, missing auth, and transport failures return structured failure payloads
  - unsupported tool names still fail without stalling the session

### 17.6 Observability

- Validation failures are operator-visible
- Structured logging includes issue/session context fields
- Logging sink failures do not crash orchestration
- Token/rate-limit aggregation remains correct across repeated agent updates
- If a human-readable status surface is implemented, it is driven from orchestrator state and does
  not affect correctness
- If humanized event summaries are implemented, they cover key wrapper/agent event classes without
  changing orchestrator behavior

### 17.7 CLI and Host Lifecycle

- CLI accepts a positional workflow path argument (`path-to-WORKFLOW.md`)
- CLI uses `./WORKFLOW.md` when no workflow path argument is provided
- CLI errors on nonexistent explicit workflow path or missing default `./WORKFLOW.md`
- CLI surfaces startup failure cleanly
- CLI exits with success when application starts and shuts down normally
- CLI exits nonzero when startup fails or the host process exits abnormally

### 17.8 Real Integration Profile (RECOMMENDED)

These checks are RECOMMENDED for production readiness and MAY be skipped in CI when credentials,
network access, or external service permissions are unavailable.

- A real tracker smoke test can be run with valid credentials supplied through the selected
  adapter's documented secret mechanism.
- Real integration tests SHOULD use isolated test identifiers/workspaces and clean up tracker
  artifacts when practical.
- A skipped real-integration test SHOULD be reported as skipped, not silently treated as passed.
- If a real-integration profile is explicitly enabled in CI or release validation, failures SHOULD
  fail that job.

## 18. Implementation Checklist (Definition of Done)

Use the same validation profiles as Section 17:

- Section 18.1 = `Core Conformance`
- Section 18.2 = `Extension Conformance`
- Section 18.3 = `Real Integration Profile`

### 18.1 REQUIRED for Conformance

- Workflow path selection supports explicit runtime path and cwd default
- `WORKFLOW.md` loader with YAML front matter + prompt body split
- Typed config layer with defaults and `$` resolution
- Dynamic `WORKFLOW.md` watch/reload/re-apply for config and prompt
- Polling orchestrator with single-authority mutable state
- GitHub Projects board adapter with state-list + project-item ID-refresh reads (Section 11.6)
- Workspace manager with sanitized, collision-resistant per-issue workspaces
- Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`)
- Hook timeout config (`hooks.timeout_ms`, default `60000`)
- Oh-my-pi RPC subprocess client with the targeted JSONL/framing protocol
- Oh-my-pi launch command config (`omp.command`, default `omp --mode rpc --no-ui`)
- Strict prompt rendering with `issue` and `attempt` variables
- Exponential retry queue with continuation retries after normal exit
- Configurable retry backoff cap (`agent.max_retry_backoff_ms`, default 5m)
- Reconciliation that stops runs on terminal/non-active tracker states
- Workspace cleanup for terminal issues (startup sweep + active transition)
- Structured logs with `issue_id`, `issue_identifier`, and `session_id`
- Operator-visible observability (structured logs; OPTIONAL snapshot/status surface)

### 18.2 RECOMMENDED Extensions (Not REQUIRED for Conformance)

- HTTP server extension honors CLI `--port` over `server.port`, uses a safe default bind host, and
  exposes the baseline endpoints/error semantics in Section 13.7 if shipped.
- Provider-native agent tools, when shipped, execute through the oh-my-pi RPC host-tool protocol using
  host-side configured adapter auth without passing tracker secrets to the child.
- TODO: Persist retry queue and session metadata across process restarts.
- TODO: Make observability settings configurable in workflow front matter without prescribing UI
  implementation details.
- TODO: Extract common semantic helper tools only after multiple adapters demonstrate real
  duplication; do not preemptively replace provider-native tools with generic CRUD.

### 18.3 Operational Validation Before Production (RECOMMENDED)

- Run the `Real Integration Profile` from Section 17.8 with valid credentials and network access.
- Verify hook execution and workflow path resolution on the target host OS/shell environment.
- If the OPTIONAL HTTP server is shipped, verify the configured port behavior and loopback/default
  bind expectations on the target environment.

## Appendix A. SSH Worker Extension (OPTIONAL)

This appendix describes a common extension profile in which Symphony keeps one central
orchestrator but executes worker runs on one or more remote hosts over SSH.

Extension config:

- `worker.ssh_hosts` (list of SSH host strings, OPTIONAL)
  - When omitted, work runs locally.
- `worker.max_concurrent_agents_per_host` (positive integer, OPTIONAL)
  - Shared per-host cap applied across configured SSH hosts.

### A.1 Execution Model

- The orchestrator remains the single source of truth for polling, claims, retries, and
  reconciliation.
- `worker.ssh_hosts` provides the candidate SSH destinations for remote execution.
- Each worker run is assigned to one host at a time, and that host becomes part of the run's
  effective execution identity along with the issue workspace.
- `workspace.root` is interpreted on the remote host, not on the orchestrator host.
- The oh-my-pi RPC process is launched over SSH stdio instead of as a local subprocess, so the
  orchestrator still owns the session lifecycle even though commands execute remotely.
- Continuation turns inside one worker lifetime SHOULD stay on the same host and workspace.
- A remote host SHOULD satisfy the same basic contract as a local worker environment: reachable
  shell, writable workspace root, coding-agent executable, and any required auth or repository
  prerequisites.

### A.2 Scheduling Notes

- SSH hosts MAY be treated as a pool for dispatch.
- Implementations MAY prefer the previously used host on retries when that host is still
  available.
- `worker.max_concurrent_agents_per_host` is an OPTIONAL shared per-host cap across configured SSH
  hosts.
- When all SSH hosts are at capacity, dispatch SHOULD wait rather than silently falling back to a
  different execution mode.
- Implementations MAY fail over to another host when the original host is unavailable before work
  has meaningfully started.
- Once a run has already produced side effects, a transparent rerun on another host SHOULD be
  treated as a new attempt, not as invisible failover.

### A.3 Problems to Consider

- Remote environment drift:
  - Each host needs the expected shell environment, coding-agent executable, auth, and repository
    prerequisites.
- Workspace locality:
  - Workspaces are usually host-local, so moving an issue to a different host is typically a cold
    restart unless shared storage exists.
- Path and command safety:
  - Remote path resolution, shell quoting, and workspace-boundary checks matter more once execution
    crosses a machine boundary.
- Startup and failover semantics:
  - Implementations SHOULD distinguish host-connectivity/startup failures from in-workspace agent
    failures so the same ticket is not accidentally re-executed on multiple hosts.
- Host health and saturation:
  - A dead or overloaded host SHOULD reduce available capacity, not cause duplicate execution or an
    accidental fallback to local work.
- Cleanup and observability:
  - Operators need to know which host owns a run, where its workspace lives, and whether cleanup
    happened on the right machine.
