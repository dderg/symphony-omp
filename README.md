# Symphony

Symphony turns project work into isolated, autonomous implementation runs, allowing teams to manage
work instead of supervising coding agents.

[![Symphony demo video preview](.github/media/symphony-demo-poster.jpg)](https://player.vimeo.com/video/1186371009?h=5626e4b899)

_In this [demo video](https://player.vimeo.com/video/1186371009?h=5626e4b899), Symphony monitors a Linear board for work and spawns agents to handle the tasks. The agents complete the tasks and provide proof of work: CI status, PR review feedback, complexity analysis, and walkthrough videos. When accepted, the agents land the PR safely. Engineers do not need to supervise Codex; they can manage the work at a higher level._

> [!WARNING]
> Symphony is a low-key engineering preview for testing in trusted environments.

## Running Symphony

### Requirements and startup

- Go 1.23 or newer to build; POSIX host with `bash`.
- oh-my-pi **18.4.12**, targeting RPC revision
  [`d19afc28611dd6fa485160659eb42a4da36022e9`](https://github.com/can1357/oh-my-pi/tree/d19afc28611dd6fa485160659eb42a4da36022e9).
  Authenticate its model provider on the host before running.
- An accessible GitHub Projects V2 board and a host-side credential with the required project and
  repository permissions. See the [adapter profile](docs/github-projects.md) for setup, normalization,
  scoped tools, permissions and errors.

```sh
go build -o symphony ./cmd/symphony
export GITHUB_PROJECT_ID='PVT_your_project_node_id'
export GITHUB_TOKEN='your_host_side_token'
export SYMPHONY_REPOSITORY_URL='https://github.com/your-org/your-repository.git'
cp WORKFLOW.example.md WORKFLOW.md
./symphony ./WORKFLOW.md
```

Without a positional argument, Symphony loads `WORKFLOW.md` from the process working directory.
Startup configuration errors exit nonzero. `SIGINT` and `SIGTERM` request orderly worker shutdown and
exit successfully; failure to stop within the host shutdown deadline exits nonzero. JSON structured
logs go to stderr, with issue/session context, action outcomes and validation failures.

The example's `after_create` hook clones a repository into each new issue workspace. Supply a trusted
repository URL and adapt the hook to your checkout policy. Symphony does not clone, reset, fetch,
commit, open PRs or land changes on its own: repository preparation belongs in hooks, and work/handoff
policy belongs in the prompt. Existing workspaces are reused without destructive reset.

The example hands completed work to **Human Review**, which must exist as a single-select board
Status option and must remain outside both `active_states` and `terminal_states`. Add that option
before running, or edit the prompt to use your chosen non-active, non-terminal handoff. This stops
agent execution while preserving the workspace's local implementation for review.

### Workflow configuration

`WORKFLOW.md` contains optional YAML front matter and a strict Liquid prompt body. Every normalized
issue field, nested labels/blockers/native references and `attempt` is available. First attempts have
`attempt: null`; later worker attempts receive a positive retry/continuation number. Known null values
render empty and retain null comparison semantics. Unknown variables/properties and filters fail the
attempt, including references in inactive branches and empty loops.

| Setting | Default / behavior |
| --- | --- |
| `tracker.kind` | Required; `github_projects` |
| `tracker.provider` | Adapter-owned object; required project node ID and host authentication |
| `tracker.active_states` / `terminal_states` | `[Todo, In Progress]` / `[Done]`; board Status options, not issue OPEN/CLOSED |
| `tracker.required_labels` | `[]`; every configured label must match, case-insensitively; blank labels match nothing |
| `polling.interval_ms` | `30000` |
| `workspace.root` | System temp directory's `symphony_workspaces`; `$VAR` and `~` supported; relative to workflow directory |
| `hooks.after_create` / `before_run` | Optional shell scripts; failure aborts preparation/attempt |
| `hooks.after_run` / `before_remove` | Optional shell scripts; failure logged and ignored |
| `hooks.timeout_ms` | `60000`; positive integer |
| `agent.max_concurrent_agents` / `max_turns` | `10` / `20`; turns count submitted Symphony prompts |
| `agent.max_concurrent_agents_by_state` | `{}`; trim/lowercase state keys; invalid/nonpositive entries ignored |
| `agent.max_retry_backoff_ms` | `300000`; failures start at 10 seconds and double to this cap |
| `omp.command` | `omp --mode rpc --no-ui`; passed unchanged to `bash -lc` |
| `omp.read_timeout_ms` | `5000`; readiness, synchronous response and prompt admission deadline |
| `omp.turn_timeout_ms` | `3600000`; **output silence**, reset by each physical RPC frame, not total runtime |
| `omp.stall_timeout_ms` | `300000`; orchestrator event inactivity; nonpositive disables detection |

Unknown configuration keys are ignored, while unknown tracker-provider keys are preserved for the
adapter. Environment variables are not global overrides: only documented `$VAR` references and
adapter credential fallbacks apply. Positive runtime integer fields are validated before startup.

Symphony checks workflow contents once per second and revalidates before dispatch/retry. Valid edits
update future polling, dispatch limits, routing, retry scheduling, prompts and agent launches. Invalid
edits keep the last good configuration for reconciliation but block new dispatch until fixed.
Unchanged watcher checks do not construct a new tracker or send validation requests. In-flight
sessions retain their adapter/authentication, command and workspace root; lifecycle-hook scripts and
timeouts can change live. A workspace-root edit affects future attempts and preserves previous local
work. Terminal cleanup removes every workspace root used for that issue in the current daemon,
including roots retained through queued retries; previous roots are not discovered across restarts.

### Execution and recovery

The scheduler has one in-memory authority for claims, running workers, retry timers and metrics.
It reconciles before each dispatch cycle, sorts priority `1..4` before unknown priorities, then oldest
creation time and identifier, and applies global/per-state limits. Candidates are refreshed by opaque
project-item ID before launch. Missing, unroutable or non-active issues stop their workers; terminal
issues also remove their workspaces **after the worker/process has stopped**. Startup sweeps terminal
workspaces. Tracker failures never turn partial pages into successful dispatch results.

**Terminal cleanup deletes the entire workspace, including uncommitted changes.** Preserve completed
artifacts externally (for example in a pushed branch/PR or an approved archive) before moving an issue
to `Done` or another configured terminal state. The default example does not commit or push without
explicit authorization, so it uses the non-terminal Human Review handoff rather than Done.

Workspace keys keep safe identifiers unchanged; changed identifiers receive a stable 64-bit SHA-256
suffix after sanitization. Trusted root aliases (including macOS `/tmp` and `/var`) resolve to physical
paths. Issue-directory symlinks, root/parent traversal and non-directory workspaces are rejected.
`after_create` runs only once; `before_run` runs per attempt; `after_run` runs whenever the workspace
exists, including failed creation hooks, failed before-run hooks and cancellation. Hooks run in the
workspace with bounded deadlines and tracker secrets removed. Their output is discarded to avoid
secret-bearing diagnostics; exit/timeout outcomes are logged.

Each worker starts a fresh issue-bound RPC session, waits for advertised readiness limits, negotiates
v2 lossless chunks, and validates order, lengths, UTF-8 and reassembly limits. Prompt acknowledgements
are not completion: matching `prompt_result` plus session quiescence is required, and background
errors fail the attempt. Local-only prompts are handled without waiting for nonexistent results.
Continuation prompts reuse the session without repeating the original prompt, and refresh complete
issue routing/labels/state between turns. Cumulative session statistics are sampled after settled
turns and before normal shutdown; repeated snapshots are deduplicated, and unavailable rate limits
remain null.

Clean exits queue a one-second continuation refresh; failures queue capped exponential backoff.
Claims prevent concurrent duplicate runs. Restart recovery is tracker/filesystem-driven: retry timers
and live session state do not persist. Cancellation sends `abort`, closes stdin and drains stdout;
shutdown allows five seconds before process-group termination, with a further one-second bounded
reap. The host gives workers fifteen seconds to finish cancellation and hooks. Cleanup hooks entered
before host cancellation inherit it; `after_run` entered after cancellation receives at most five
seconds of detached grace. Startup/terminal `before_remove` hooks cannot delay shutdown for their
full configured timeout.

### Trust and operator policy

This implementation targets **trusted workflows on a trusted host**. Workspace cwd and path checks
are not an OS sandbox. `omp.command` must not redirect cwd; known cwd-changing shell commands and
CLI options are rejected without rewriting the command. Scripts, extensions and agent tools can
still access host resources according to OS permissions. Use a dedicated user/container/VM,
restricted mounts/network and appropriate oh-my-pi tool policy for stronger isolation.

Headless `--mode rpc --no-ui` does **not** mean automatic approval: oh-my-pi tools requiring
interactive approval fail closed. RPC dialog requests are cancelled and fail the attempt rather
than hanging; presentation notifications remain observability-only. No interactive operator UI is
implemented.

Tracker tools execute host-side, are scoped to the current issue/project, and return results rather
than credentials. Never put literal tracker tokens in a workspace-readable workflow. Child and hook
environments remove the adapter's declared tracker-secret names. Host-tool cancellation propagates
to provider requests, but an accepted mutation cannot be undone and may have an ambiguous outcome;
inspect GitHub before retrying comments. Model-provider credentials remain the agent's responsibility.
Production HTTPS retains Go's native certificate validation; there is no insecure fixture bypass.

Optional HTTP/dashboard/`--port`, SSH workers, persistent scheduling state and built-in VCS population
are intentionally not implemented. The Go `Snapshot(ctx)` / `Refresh(ctx)` API offers an optional
operator integration, with bounded waits and unavailable/timeout errors; orchestration does not
depend on it.

### Validation evidence

```sh
go test -race -v ./...
```

The deterministic standalone profile builds the **unmodified production executable** inside Linux,
trusts an ephemeral fixture CA through native `SSL_CERT_FILE`, and exercises real GraphQL HTTPS,
shell hooks, RPC peer processes, scheduler/reload/retry/restart behavior and signal shutdown. It
passed twice under the race detector with four CPUs and 4 GiB available:

```sh
docker run --rm --init --cpus=4 --memory=4g \
  -v "$PWD":/src -w /src golang:1.25 bash -c \
  'go build -o /tmp/symphony-built ./cmd/symphony &&
   SYMPHONY_STANDALONE_BINARY=/tmp/symphony-built go test -race -count=2 -v ./...'
```

The installed oh-my-pi 18.4.12 and authenticated model were also exercised with a freshly rebuilt
native standalone Symphony binary on the final frozen core source. With the exact shipped
`omp --mode rpc --no-ui` command and `read_timeout_ms: 5000`, the race-enabled smoke passed in
30.69 seconds: actual file creation, scoped Status transition to Done, settled turn, completed worker,
archived proof, terminal workspace removal and successful SIGTERM shutdown. Only synthetic board
data and a random synthetic bearer token crossed the temporary public-certificate HTTPS tunnel;
host GitHub/model credentials were not sent through it. The fixture waits for public DNS publication
before its first hostname probe, preventing premature negative caching without changing production
DNS or TLS settings. Actual approval-policy and pending-provider cancellation checks ran separately.

A live authenticated GitHub schema probe initially succeeded while Projects reads were blocked by
missing credential scope. After project authorization on 2026-10-03, the production daemon read an
actual private board and dispatched its issue through the native agent. The issue had an empty body:
the agent posted a clarification through `github_add_comment`, moved Status to Human Review through
`github_set_status`, and reconciliation stopped the worker while preserving its workspace. No ticket
implementation or file edits were claimed; live close/reopen mutations were not exercised.
The legacy CLI-main test-binary smoke remains a separate integration check, not evidence for the
standalone binary. See [the complete validation matrix and evidence](docs/e2e-validation.md) for
commands, profile boundaries, tested root-cause fixes and external limits. Passing these checks does
not prove that every possible deployment or failure is bug-free.

Protocol references:
[pinned RPC documentation](https://github.com/can1357/oh-my-pi/blob/d19afc28611dd6fa485160659eb42a4da36022e9/docs/rpc.md),
[pinned wire types](https://github.com/can1357/oh-my-pi/blob/d19afc28611dd6fa485160659eb42a4da36022e9/packages/coding-agent/src/modes/rpc/rpc-types.ts),
[pinned package version](https://github.com/can1357/oh-my-pi/blob/d19afc28611dd6fa485160659eb42a4da36022e9/packages/coding-agent/package.json).

---

## License

This project is licensed under the [Apache License 2.0](LICENSE).
