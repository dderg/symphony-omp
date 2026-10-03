# End-to-end validation

## Profiles and observed results

The deterministic release profile exercises an **unmodified standalone Symphony executable**, not a CLI function called by a Go test helper. Linux's native certificate loader trusts a temporary fixture CA through `SSL_CERT_FILE`. The synthetic GraphQL server requires a synthetic bearer token. RPC peers are independent subprocesses: stdio framing, shell invocation, cwd, EOF, cancellation, hooks, workspaces, timers and process groups are real. No production TLS bypass, host trust-store edit, production test adapter or scheduler/runner substitution is used.

```sh
docker run --rm --init --cpus=4 --memory=4g \
  -v "$PWD":/src -w /src golang:1.25 bash -c \
  'go build -o /tmp/symphony-built ./cmd/symphony &&
   SYMPHONY_STANDALONE_BINARY=/tmp/symphony-built go test -race -count=2 -v ./...'
```

Observed final result: both repetitions passed; root package `142.670s`, command package `1.017s`, wall time `154.15s`. The full internal output is `artifact://89`. This includes all release scenarios below, the real public-Go-API consumer subprocess, existing deterministic checks, and the expanded faithful-provider HTTPS tests. Live-model tests explicitly skip unless enabled; their separate successful profile is not silently included in the container result.

A Linux host can use the same profile without Docker:

```sh
go build -o /tmp/symphony-built ./cmd/symphony
SYMPHONY_STANDALONE_BINARY=/tmp/symphony-built go test -race -count=2 -v ./...
```

The native actual-agent profile uses installed oh-my-pi **18.4.12** and an authenticated model. It has been exercised both through `RunAttempt` and through a separately built native release binary:

```sh
go build -o /tmp/symphony-native-e2e ./cmd/symphony
SYMPHONY_ACTUAL_OMP=1 SYMPHONY_ACTUAL_OMP_BINARY=/tmp/symphony-native-e2e \
  SYMPHONY_ACTUAL_OMP_COMMAND='omp --mode rpc --no-ui' \
  go test -race -run '^TestExternalActualOMPModelAndGitHubHandoff$' -v -count=1 .
SYMPHONY_ACTUAL_OMP=1 \
  go test -race -count=2 -run '^TestExternalActualOMP' -v .
```

The standalone native check creates an exact-content file with the real model, performs a real host-side scoped GraphQL Status mutation against a synthetic board, waits for settled completion and worker completion, archives the file outside the workspace before terminal deletion, observes terminal workspace removal, and verifies successful SIGTERM shutdown. A temporary `cloudflared` HTTPS tunnel provides normal public certificate trust on macOS; only synthetic data and a random synthetic bearer token cross it. No real GitHub or model credential is sent to the tunnel. The tunnel and test workspaces are removed by test cleanup.

**Final-current-source native proof:** after the core freeze and successful repeated Docker suite, a fresh native build passed the race-enabled smoke in **30.69s** (`31.988s` package time, `32.84s` wall time). It used the exact shipped `omp --mode rpc --no-ui` command and `read_timeout_ms: 5000`, without model, approval, tool, extension, skill or rule overrides. The observed sequence required the exact archived proof file, scoped Status transition to Done, `turn_completed`, a completed worker with null error/empty reason, terminal workspace absence, and successful SIGTERM. The authoritative command and output are recorded at `local://native-default-frozen-output.txt` (external job `bg_29`). Earlier corrected cleanup repetitions (`17.75s`, `92.17s`) are recorded separately at `local://native-cleanup-repeat-output.txt`; they are not substituted for the final-source proof.

The external fixture admits its newly issued hostname only after public DoH A-record publication, before making its first native hostname HTTPS probe. This prevents fixture-induced early negative DNS caching. Earlier attempts that failed this admission never launched Symphony and are not counted as runtime successes. The final connector used HTTP2; changing connector transport alone had not resolved publication timing. No DNS override, TLS bypass, global trust-store edit or production DNS/TLS configuration change was introduced.

Installed-agent checks also exercised host-default headless policy, explicit `always-ask` refusal confirmed by the actual transcript's failed write tool result, and cancellation of an actual model's pending provider request. Headless mode alone is not an approval override.

A live authenticated GitHub GraphQL schema probe initially succeeded, including the selected archived-item, pagination and mutation wire contracts, while a ProjectV2 read failed with `INSUFFICIENT_SCOPES` because the credential lacked `read:project`. That was the original permission gap, not a passed project smoke.

After project authorization on **2026-10-03**, a freshly built production daemon read an actual private GitHub board and dispatched an open, unlocked, unarchived issue through installed native oh-my-pi and the authenticated model. The issue had an empty body and no implementation requirements. The agent itself posted one clarification through `github_add_comment` and moved board Status to **Human Review** through `github_set_status`; authenticated provider reads confirmed both results. The turn settled, and reconciliation stopped the worker as ineligible for the non-active, non-terminal handoff while preserving the workspace. No files were edited, no ticket implementation or ticket tests were claimed, and the daemon remained running. This live board/agent evidence is separate from the synthetic release and native proof-file profiles above; real close/reopen mutations were not exercised. No test suite can prove the absence of every possible bug or deployment-specific failure.

## Test names and evidence levels

- **Release**: `TestStandaloneRuntimeEndToEnd`, `TestStandaloneStartupFailures`, `TestStandaloneAdversarialScenarios/<mode>`, `TestStandaloneSchedulerPipeline/<scenario>`. Each launches the production binary.
- **API process**: `TestRuntimeSnapshotProcess`, driven by `snapshot-api`, calls public `NewOrchestrator`, `Run`, `Snapshot` and `Refresh` in a separate consumer process, using the real adapter, real actor, real hooks and real RPC subprocess. It is not a CLI HTTP endpoint.
- **Provider HTTPS**: `TestGitHub*` and `TestExternalGitHub*` call the real public adapter/tools against authenticated TLS fixtures. They are adapter integration checks, not standalone daemon E2E claims.
- **Deterministic core**: named loader/template/scheduler/workspace/RPC checks isolate additional boundary semantics. Shell/workspace checks use real files and shell processes; direct scheduler state fixtures remain isolated tests, not E2E.
- **Actual agent/model**: `TestExternalActualOMP*`, enabled explicitly. The binary variant adds native standalone CLI integration.
- The older `TestSpecCLIMainTLSProcessSmoke` builds the command's Go test executable and installs its fixture CA in a test-only HTTP transport. It remains useful, but is not release-binary TLS evidence.

## §17.1 Workflow and configuration

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| Explicit workflow path wins | Release `TestStandaloneRuntimeEndToEnd`; scheduler scenarios invoke the positional path |
| Default cwd `WORKFLOW.md` | Every release adversarial scenario invokes the binary without a positional path |
| Content changes detected/reapplied without restart | Release lifecycle failure→retry renders the reloaded prompt; `capacity-reload-labels` reapplies live limits; `retry-root` / `retry-root-follow` reapply root |
| Invalid reload retains good reconciliation config and emits error | `capacity-reload-labels`: invalid YAML blocks new work while a running issue transitions to Human Review; repair restores dispatch |
| Typed missing-file error | Release startup cases for missing default and explicit file assert `missing_workflow_file` |
| Typed invalid YAML error | Release startup and invalid-reload cases assert `workflow_parse_error` |
| Typed non-map front matter error | Release startup asserts `workflow_front_matter_not_a_map` |
| Optional defaults | Release `defaults` omits max turns, polling and state lists; observes 20 submitted prompts; `TestWorkflowLoading` checks every typed default |
| Supported tracker kind validation | Release unsupported-kind startup case; `TestGitHubConfigAndErrors` validates provider construction |
| Adapter-owned provider keys preserved/validated | `TestWorkflowLoading` preserves unknown provider keys; real constructor validation in `TestExternalGitHubConstructorFieldValidation` and `TestGitHubConfigAndErrors` |
| Documented secret/project/path environment references | Release provider auth uses `$RUNTIME_TRACKER_SECRET`; `defaults` resolves project ID; `env-path` resolves workspace root |
| Home path expansion | Release `tilde-path` |
| Shell command string preserved | Release commands traverse actual `bash -lc`; `TestWorkflowLoading` checks shell quoting/variable text remains unchanged |
| Per-state names normalized; invalid entries ignored | `capacity-reload-labels` supplies `' ToDo ': 1`, real per-state capacity and invalid negative entry; `TestWorkflowLoading` checks typed normalization |
| Issue and attempt rendered | Lifecycle rendered identifier; `backoff` observes first null then retry attempts 1 and 2; `null-template` preserves known null values |
| Strict unknown variables | Release `strict-template`, `inactive-template`; `TestStrictTemplates` covers inactive branches, empty loops and dynamic/nested properties |

Positive integer boundaries are also exercised as release startup failures for hook timeout, max turns and poll interval. Unknown filters fail the actual worker in `unknown-filter`. Relative path resolution is exercised by `relative-path`.

## §17.2 Workspaces, hooks and launch safety

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| Deterministic path; missing directory created | All release peers record physical issue cwd; hook markers and workspace files prove creation |
| Existing workspace reused | `restart`: two daemon lifetimes, one after-create hook, preserved local file |
| Existing non-directory handled safely | Release `non-directory`: attempt fails and original file content survives |
| Population/preparation errors surface | Release `create-failure`, `create-timeout`, `before-failure`, `before-timeout` |
| after_create only on new workspace | Lifecycle retry and `restart`; one creation marker for reused issue |
| before_run every attempt; fatal failures/timeouts | Lifecycle/retry hook effects; `before-failure`, `before-timeout` |
| after_run on success/failure/cancel; ignored failures/timeouts | Successful modes, failed modes and reconciliation/shutdown modes; `after-failure`, `after-timeout` retain successful worker outcome |
| before_remove executes; ignored failure/timeout | `terminal`, `remove-failure`, `remove-timeout`; all observe removal after stopped worker |
| Sanitization, stable hash, root containment before launch | Release sanitized GitHub identifiers/cwd, `symlink`, `non-directory`, `cwd-override`, `root-alias`; `TestWorkspaceKeys` / `TestWorkspaceFailureAndContainment` directly test the public generic key/path contract |
| Safe identifiers unchanged; sanitization collisions produce distinct keys | `TestWorkspaceKeys` exercises `safe._-1`, `a/b`, `a#b` with deterministic, distinct ≥64-bit suffixes. This generic identifier boundary is a direct workspace check, not a fabricated GitHub issue format |
| Actual agent cwd; out-of-root launch rejected | Recorded release peer cwd plus `cwd-override`; direct path validation checks reject root/parent escape and symlinks |

Hook timeout and shutdown scenarios record shell process-group leaders; release checks verify hook and peer process groups no longer exist after daemon exit. `after-long-shutdown`, `shutdown-long`, `remove-long-shutdown`, and `startup-cleanup` cover cancellation during long hooks. `TestTerminalCleanupHonorsHostCancellation` additionally requires the actual shell-start marker, then checks deletion and shell disappearance. Ignored hook validation/start failures have explicit operator-visible warning checks in `TestIgnoredHooksLogValidationAndStartFailures` rather than being mislabeled as release scenarios.

## §17.3 Tracker integration

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| Candidate active states and configured scope | Release scheduler/lifecycle fixtures; `TestGitHubTLSGraphQLAndTools` checks real scoped reads |
| Empty state and ID inputs make no provider call | Request-count assertions in `TestGitHubTLSGraphQLAndTools` |
| Multiple-page order and completeness | `TestGitHubTLSGraphQLAndTools`, `TestGitHubPagedFieldValidationAndArchivedTerminalVisibility` |
| Lowercase/trim/deduplicated labels | Provider HTTPS normalization checks; release required-label dispatch/continuation |
| Invalid optional metadata falls back to null/empty | `TestExternalGitHubRequiredOptionalAndVisibility/optional_nulls`, `TestGitHubAtomicRefreshAndEligibility` |
| Malformed list omission logged; malformed requested refresh fails | `TestGitHubAtomicRefreshAndEligibility`, required-field external matrix including missing Status |
| Full refresh by opaque dispatch ID | Provider HTTPS checks plus every release candidate revalidation/reconciliation |
| Distinct provider IDs retained in native_ref | `TestGitHubTLSGraphQLAndTools`; real scoped host-tool execution uses project-item and underlying issue IDs |
| Provider routing explicitly sets dispatchable | Archived/closed/locked provider checks; release `ineligible`, unroutable candidate and label cases |
| Compact adapter profile | [GitHub Projects profile](github-projects.md) documents config, pagination, normalization, tool scope and errors |
| Config/request/status/payload/pagination/rate-limit category and message mapping | `TestGitHubConfigAndErrors`, `TestGitHubPaginationIntegrity`, `TestExternalGitHubRedirectAndRateLimitIsolation`, `TestExternalGitHubConstructorFieldValidation` |
| Project ID/auth/Status field validation | Release env/auth paths; real constructor field-option matrix |
| Board Status, not OPEN/CLOSED, drives workflow | Provider fixture OPEN issues with terminal board Status and closed active items; release Status mutation/terminal cleanup |
| Multi-repository identity and distinct item/issue IDs | `TestGitHubTLSGraphQLAndTools`, `TestGitHubAtomicRefreshAndEligibility` |
| PR/draft/inaccessible excluded; archived/closed not dispatchable | Provider HTTPS visibility/eligibility matrices; real daemon archived reconciliation |
| Missing Status follows malformed record rules | Required-field matrices, not synthetic OPEN/CLOSED fallback |
| Out-of-project refresh rejected; pagination/GraphQL errors are atomic | `TestGitHubAtomicRefreshAndEligibility`; four connection types × repeated cursor/late error/disappearance in `TestExternalGitHubConnectionFailuresAreAtomic`; release `provider-atomic` proves no partial-page dispatch then real recovery |

`refresh-error` keeps the actual running peer alive across repeated provider refresh failures, then cancels it only after a successful non-active refresh. Redirect tests verify no credential-bearing follow-up request. Mutation malformed-result tests cover null, empty, scalar and foreign IDs for all shipped mutations.

## §17.4 Scheduling, reconciliation and retry

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| Priority then oldest creation time | `capacity-reload-labels` supplies reverse chronological provider order and observes old issue dispatch first. `TestSchedulerEligibilitySortingAndCapacity` checks generic priorities 1–4/unknown and identifier ties; GitHub profile MUST emit null priority, so a test-only production tracker was not added |
| dispatchable=false not eligible | Release archived candidate and `ineligible` active reconciliation |
| Required labels case-insensitive after normalization | `capacity-reload-labels` config `' BUG '` matches normalized `bug`; label removal stops worker; `blank-label` dispatches none |
| Active refresh updates running state | `capacity-reload-labels` changes old Todo worker to In Progress and observes Todo capacity released without ending that worker |
| Non-active stops without deletion | `inactive`, label-removal and refresh-error recovery; preserved workspace checked |
| Terminal stops then cleans | `terminal`, remove hook variants and actual-agent terminal cleanup |
| Empty running reconciliation no-op | `blank-label` / startup cleanup idle cycles; direct scheduler no-running checks |
| Normal exit schedules attempt-1 short continuation | `restart`, `retry-root`, `retry-root-follow`; API process asserts attempt 1; normal lifecycle peer max-turn completion |
| Abnormal exit increments 10s exponential retries | `backoff` observes real launches after 10s and 20s, with rendered attempts 1 and 2 |
| Configured retry cap | `backoff` observes attempt 3 queued at 25000ms rather than 40000ms; lifecycle smoke exercises actual 1500ms capped retry |
| Retry entry attempt/due/identifier/error | Public API consumer and `TestSchedulerSnapshotUnavailableTimeoutAndRows`; release retry logs expose attempt/delay/reason with issue context |
| Stall kill and retry | Release `stall`; isolated live stall-setting reload regression |
| Slot exhaustion requeue explicit error | `retry-slots` observes `no available orchestrator slots`, no duplicate run, and retry dispatch after actual slot release |
| Snapshot rows/tokens/rate limits | `snapshot-api`: public API consumer with real adapter/runner/process, running and retry rows, cumulative 11/7/19 totals and null rate limits |
| Snapshot timeout/unavailable | API process before/after Run and cancelled request; `TestSchedulerSnapshotUnavailableTimeoutAndRows` exercises bounded unavailable/timeout paths |

`capacity-reload-labels` also verifies global/per-state limits, claims, invalid reload suppression, repaired reload and actual label eligibility. `restart` preserves filesystem work without restoring transient state. `retry-root` cleans a queued attempt's old root; `retry-root-follow` preserves old work while the next attempt uses a new root, then cleans both known roots on terminal transition.

## §17.5 RPC and host tools

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| bash/cwd and tracker-secret filtering | Every release peer validates cwd and absence of declared/GitHub secret names; actual hooks use the same production environment filtering |
| ready/limits/v2 negotiation | Actual peers advertise reduced limits; `chunks`, physical/logical/outbound overflow cases; actual installed OMP profile |
| Chunk order/length/UTF-8/interruption rejection | `bad-index`, `bad-length`, `bad-utf8`, `chunk-interrupted`, `chunk-id`, `chunk-count`, `chunk-order`, `chunk-base64`, `chunk-eof`; additional malformed numeric/nested bounds in `TestRPCDecoder` |
| Fresh issue-bound session; cancelled transition fails | PID-distinct peer sessions and native installed OMP; `cancel-startup` |
| Native session + generated prompt metadata | Structured release outcomes; real peer request IDs/session IDs; actual OMP events |
| Correlation despite interleaving | `interleaved` ignores unrelated failed response/result; `delayed-prompt-error` fails the known current prompt during get_state; direct deferred-result check |
| Acknowledgement is not completion | `ack-only` receives admission but no matching result and times out |
| Local-only prompt does not await nonexistent result | `local` |
| Success/error/abort statuses differ | Normal modes, `turn-error`, `aborted` |
| Pending background work waits; background failure fails | `background`, `background-error`; delayed current-prompt error |
| Same process/session continuation, no original prompt repeat | `continuation` observes exactly two prompts in one process; peer rejects original-task resubmission |
| Response/silence/stall deadlines distinct | `admission-timeout`, `ack-only`, `stall`; `chunks-silence` runs longer than total silence cap with each physical chunk refreshing it |
| stdout/stderr separated | `stderr` emits non-JSON diagnostics without corrupting protocol |
| Headless/dialog policy fails closed when interactive approval required | `dialog`; installed actual-agent default and explicit always-ask checks |
| Cancellation abort; bounded EOF drain/shutdown | Shutdown/reconciliation scenarios record abort; EOF/chunk EOF and process groups; direct early-EOF and trailing-error shutdown checks |
| Cumulative authoritative usage; unavailable rate limits null | API process checks repeated final snapshots remain 11/7/19, not doubled; `TestSchedulerFIFOFinalUsageAndNativeDedup`; actual-agent positive tokens |
| Host tools registered before prompt | Peer log contains real adapter tool definitions; actual OMP host call |
| Host result/error IDs and pending host_tool_cancel | Lifecycle real scoped mutation result correlation; `tool-errors`; `tool-cancel` waits for actual provider HTTP request and verifies HTTP context cancellation before settled completion |
| Only selected adapter tools advertised | Real GitHub tool definitions in release peer/actual-agent session; schema tests |
| Valid inputs host-side with adapter credential | Lifecycle Status mutation and actual model handoff; authenticated provider requests |
| Issue/native_ref internal context available | Real scoped mutation uses configured project/item/underlying issue references; explicit context regression |
| Secrets absent from child | Release peer and hook secret-isolation checks; actual host-side tool credential remains server-side |
| Invalid args/auth/transport produce structured failures | `tool-errors` tests invalid shape and unsupported name; provider HTTPS config/auth/transport and malformed mutation result checks |
| Unsupported names do not stall | `tool-errors` completes after correlated structured error results |

The client supports fresh sessions, not arbitrary resume configuration; no unbound history is adopted. Native rate-limit telemetry is unavailable and remains null rather than invented.

## §17.6 Observability

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| Validation failures operator-visible | Release startup typed categories and invalid workflow reload |
| Structured issue/session context | All release terminal-outcome checks use issue_id, outcome, reason/error and session_id; they do not pin message prose |
| Log sink failure nonfatal | `log-sink-failure` directs production stderr to `/dev/full`; real hooks/peer still complete and daemon exits normally |
| Repeated token updates aggregate once; absent rate limits null | API consumer process and direct cumulative/native dedup boundary tests |
| Human-readable surface/humanized event summaries | N/A: not shipped. Structured agent event names do not gate scheduling |

## §17.7 CLI/host lifecycle and §17.8 external profile

| Mandatory behavior | Concrete exercised check |
| --- | --- |
| Positional workflow | Release lifecycle/scheduler/native actual-agent profile |
| Cwd default workflow | Every release adversarial mode |
| Missing explicit/default error | Release startup cases |
| Clean startup failure surface | Typed-category release startup matrix |
| Successful normal startup and shutdown exit | Every release scenario observes process exit success after SIGTERM or API graceful stop |
| Nonzero startup/abnormal host exit | Release startup matrix; direct host deadline error paths are deterministic boundaries, not a promise to survive SIGKILL |
| Valid-credential real tracker smoke | After authorization on 2026-10-03, actual private-board read, native daemon dispatch, agent-authored comment and Status handoff confirmed by provider reads; close/reopen remain fixture-only |
| Isolated integration identifiers/resources and cleanup | Ephemeral release roots, synthetic project/items/tokens, archived actual-model proof, cleaned tunnel/processes; separately authorized live board/daemon retained for human clarification |
| Skipped real integration reported, not treated as pass | Actual-model tests opt in and call Skip otherwise; original GitHub scope gap and later authorized live evidence are distinguished above |
| Enabled profile failures fail validation | Ordinary Go test failures; no fallback that turns actual-agent failures into successful mocked runs |

## §18 definition of done and selected policy

All required §18.1 components map to the corresponding rows above: explicit/default workflow, YAML/config/default/environment loading, reload, one actor, GitHub ProjectV2 reads, workspace key/path safety, four hooks/deadlines, RPC JSONL/v2 client, configured shell command, strict prompt/attempts, exponential+continuation retry, reconciliation, startup/active terminal cleanup and structured logs.

Shipped §18.2 extensions are provider-native GitHub host tools and public Go Snapshot/Refresh. HTTP/dashboard/port, SSH workers, persistent retries, configurable observability and generic tracker CRUD are not shipped and are not claimed tested extensions. §18.3 POSIX hook/path behavior is exercised natively and in Linux; the external-model profile uses an installed authenticated provider. Real GitHub board reads, comment creation and Status handoff were subsequently exercised after authorization; deployments still require their own project/repository permissions, and live close/reopen remain untested.

Root-cause fixes demonstrated during this workstream:

1. Terminal cleanup ignored host cancellation during a long before_remove hook. Cleanup now retains cancellation, including during startup. The actor disables its cancellation select arm without stripping cancellation from its context. after_run entered after cancellation has a five-second detached grace; already-running hooks inherit host cancellation.
2. Workspace-root reload could orphan prior attempt workspaces during queued retry or after a new-root retry launched. The actor retains known workspace managers per issue in this daemon; terminal cleanup covers every known root only after workers stop. Root cutover itself preserves local work. No cross-restart discovery/database was added.
3. Unrelated failed RPC responses could fail the active prompt. Errors are correlated to the synchronous request or known current turn, including delayed turn failure during state/stat requests. Unrelated IDs do not fail the turn; malformed response schemas still fail.
4. Malformed/scopeless GitHub mutation responses could be advertised as successful. The adapter now validates the selected nested payload and expected item/issue identifiers/state/comment fields before returning success.

The suite asserts consumer-visible results: real files/hook effects, real provider requests/cancellation, correlated protocol outputs, public API state, error categories, process termination and exit status. Direct isolated checks remain explicitly labeled; the release suite is not a relabeling of them.
