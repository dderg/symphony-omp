# GitHub Projects adapter

`tracker.kind: github_projects` schedules accessible GitHub issues attached to one ProjectV2 project. Visual view filters and Projects classic are not used. The implementation uses Go's HTTP/JSON standard library.

## Configuration and credentials

`tracker.provider` accepts:

| Key | Meaning/default |
| --- | --- |
| `project_id` | Required nonempty ProjectV2 GraphQL node ID; supports `$VAR_NAME`. Not the numeric project URL number. |
| `endpoint` | HTTPS GraphQL URL, default `https://api.github.com/graphql`; GitHub Enterprise endpoints supported. Embedded credentials and fragments rejected; redirects are not followed. |
| `api_key` | Host-side secret string or `$VAR_NAME`. If omitted, first nonempty `GH_TOKEN`, then `GITHUB_TOKEN`. Explicit empty or unresolved values fail without fallback. |
| `status_field` | Exact single-select field name, default `Status`. Missing, ambiguous or non-single-select fields fail validation. |

Unknown provider keys are retained by core configuration but have no adapter behavior. Wrong value types are configuration errors. The constructor resolves environment references through its supplied environment lookup (default `os.LookupEnv`), validates the project and fields, and validates all configured active/terminal state names against Status options using trimmed, case-insensitive comparison. Core defaults active states to `Todo`/`In Progress` and terminal states to `Done`; other columns require explicit configuration.

Use host-side secret references, not literal credentials in workspace-readable workflow files. The adapter declares `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`, and any environment name explicitly referenced by `api_key`; the launcher removes these from child/hook environments. Authentication never appears in issue context or tool definitions. The host HTTP client uses a 30-second timeout by default; requests also honor caller context deadlines/cancellation.

Classic tokens require `read:project` for reads or `project` for mutations, plus repository permissions for private issue content. Fine-grained tokens and GitHub Apps require corresponding Projects/repository permissions. Project visibility does not imply private issue visibility.

## Reads and normalization

State-list reads scan `node(project_id).ProjectV2.items`, fully paginate the board, and filter Status locally. Refresh reads resolve project-item IDs in batches of at most 100, verify membership, and return full snapshots. Input IDs are a set. Empty state/ID lists make no requests. Project fields, item field values, issue labels and assignees are fully paginated, at most 100 nodes per page. Single-select options are a schema list, not a connection. Missing or repeated continuation cursors fail the entire call. Any transport/page/GraphQL error discards accumulated results.

The board query explicitly includes both `ARCHIVED` and `NOT_ARCHIVED`; GitHub otherwise defaults to non-archived items only.

Only accessible Issue content is normalized. Pull requests, drafts, deleted/inaccessible content and foreign/removed project items are omitted. The dispatch ID is the project-item ID, distinct from the issue ID. Identifier is `owner/repository#number`. `native_ref` contains only `project_id`, `project_item_id`, `issue_id`, `repository` (`owner/name`), and `issue_number`.

Title/body/URL/timestamps come from the issue. Invalid optional timestamps become null; valid ones parse RFC3339. Labels are trimmed, lowercased, deduplicated and blank labels discarded. Assignee is the lexicographically smallest assignee node ID or null; there is no assignment filter. Priority and branch are null, blockers are an empty list: no relationships are inferred from labels/text. All normalized fields are present, with empty collections rather than null.

Workflow state is the selected board Status spelling, never issue OPEN/CLOSED. Closing an issue does not change board Status. Dispatchable requires an unarchived project item and an open, unlocked issue. Readable archived, closed and locked items remain available with dispatchable false, including terminal cleanup reads. Core separately enforces required labels, workflow state, claims and capacity.

Malformed required records are omitted with an operator warning in state-list reads; requested malformed records fail ID refresh rather than being mistaken for invisible items. Missing Status is malformed. Unusable nullable metadata does not invalidate otherwise usable records.

Omission warnings include `issue_id` and a derived `issue_identifier` when the repository/number are usable; an unavailable identifier is logged as an empty string. No raw provider payload or credential is logged.

## Host tools

Tools are bound to the adapter/configuration/authentication snapshot of the session. They accept no target/project/repository/endpoint/query arguments. Every operation freshly resolves the current `Issue.ID` project item, verifies configured-project membership and accessible Issue content, and derives the underlying issue ID from that provider response; `native_ref` is not trusted as authorization or a mutation target.

| Tool | Exact object arguments | Operation |
| --- | --- | --- |
| `github_get_issue` | `{}` | Return refreshed normalized issue snapshot. |
| `github_set_status` | `{"status":"Human Review"}` | Resolve trimmed/case-insensitive option name and call `updateProjectV2ItemFieldValue` with project/item/field IDs and `singleSelectOptionId`. |
| `github_add_comment` | `{"body":"Ready for review"}` | Call `addComment` using verified underlying issue ID. Nonblank body required. |
| `github_set_issue_state` | `{"state":"OPEN"}` or `{"state":"CLOSED"}` | Call `reopenIssue` or `closeIssue` on verified underlying issue ID. |

Additional properties, wrong types, missing/blank required arguments and unsupported names fail. Tool definitions contain valid object JSON Schemas, including an empty required array for the no-argument read. No arbitrary GraphQL is exposed. Board transitions and issue closure/reopening are independent.

Results provide JSON text content and JSON-safe details. Failures carry structured `error.category`/`error.message` and `IsError=true` (placed on the outer RPC result by core). Tools use the configured host credential, not child authentication. Comments are not idempotent; repeating a successful call adds another comment. State-setting operations express the requested state but are not automatically retried. There is no automatic request retry or rate-limit sleep.

Mutation success requires the schema-selected nested result: the configured item ID for Status, a nonempty comment ID/URL, or the verified issue ID and requested OPEN/CLOSED state. Missing, null, scalar, incomplete or mismatched results produce `tracker_response` with `IsError=true`; an HTTP-200 response containing an arbitrary object is not success evidence. As with cancellation, an accepted mutation may have completed before an invalid response was received.

RPC cancellation is passed through the request context. A mutation already accepted by GitHub cannot be rolled back; cancellation/timeouts may leave an unknown outcome. Inspect the provider before retrying, particularly for comments. Project membership is checked immediately before the mutation request; GitHub does not provide a transactional membership-and-issue-mutation guard, so removal racing that request cannot be atomically prevented.

## Errors and proof

Public tracker errors are `*Error` with stable `Category` and safe `Message`; `Error()` exposes category/message. Categories: `unsupported_tracker_kind`, `invalid_tracker_config`, `missing_tracker_secret`, `tracker_request` (transport), `tracker_status` (non-success HTTP), `tracker_response` (invalid data or non-rate-limit GraphQL errors), `tracker_pagination`, and `tracker_rate_limited`. HTTP 429, rate-limit-marked 403 and GraphQL rate-limit types/codes are classified as rate limiting. Provider error bodies/messages and transport diagnostic strings are not exposed, preventing token echo in errors. Context cancellation/deadline errors propagate to read callers; tools translate them into `cancelled`/`timeout` failures. Tool-only categories also include `invalid_arguments`, `unsupported_tool` and `out_of_scope`.

Deterministic adapter proof runs with `go test -v -run 'TestGitHub|TestExternalGitHub' .`. The local TLS smoke exercises authenticated GraphQL documents and variables, all four mutation paths (Status, comment, close and reopen), fresh provider issue-ID derivation and foreign-item rejection. Fixtures return the actual schema-selected mutation fields, not synthetic `ok` echoes. External transport cases exercise late errors, disappearing nodes and repeated cursors for board items, field values, labels and assignees; constructor field/option/state failures; redirect credential isolation; primary/secondary/GraphQL rate limits; malformed trailing JSON; required versus nullable metadata and removed/inaccessible/PR content. Errors never return partial records or echo synthetic tokens.
 
Live GitHub read-only introspection on 2026-10-03 confirmed `ProjectV2.items.archivedStates`, field-value cursor arguments, `ProjectV2FieldValue.singleSelectOptionId: String`, and the Status/comment/close/reopen input identifiers used by this adapter. Initially the installed `gh` credential had repository access but lacked `read:project`/`project`, and a real `viewer.projectsV2` query returned `INSUFFICIENT_SCOPES`. No resources or permissions were changed during that initial read-only probe; schema-shaped HTTPS fixtures supplied the original project/mutation evidence.

After project authorization later on **2026-10-03**, a separately authorized private-board run exercised actual authenticated board reads and production-daemon dispatch through native oh-my-pi and the authenticated model. The issue had an empty body, so the agent posted a clarification through its registered `github_add_comment` host tool and moved Status to **Human Review** through `github_set_status`, rather than inventing an implementation. Provider reads confirmed the comment and Status; the turn settled and reconciliation stopped the worker for the non-active handoff while preserving its workspace and leaving the daemon running. No ticket files were edited or ticket tests claimed. This closes the original read/Status/comment permission-evidence gap, but does not claim live close/reopen coverage; those paths remain covered by the authenticated HTTPS fixtures.
 
The opt-in `SYMPHONY_ACTUAL_OMP=1 go test -v -run '^TestExternalActualOMP' .` exercises installed oh-my-pi 18.4.12 against an authenticated model in isolated temporary workspaces. The model wrote the exact requested file, called the real registered `github_set_status` host tool against the synthetic TLS board, settled its prompt and reported positive cumulative token statistics. A separate pending-host-call check cancelled the provider HTTP context and bounded actual-agent shutdown. Native `host_tool_cancel` frame injection remains the deterministic protocol-peer check, not a claim about an observed model-generated cancellation frame.
 
The file/handoff check explicitly opts into `--approval-mode yolo` with only the built-in `write` tool enabled; this is **not** Symphony's production default. The default headless invocation was separately exercised without an approval override and wrote the test file under this host's configuration. An explicit supported `--approval-mode always-ask` run produced a real `write` tool-result error, wrote no file, and settled without interactive input. Global agent configuration was not modified.

The **exact shipped command** was separately exercised via `SYMPHONY_ACTUAL_OMP_COMMAND='omp --mode rpc --no-ui'`, with the shipped five-second readiness/command-response deadline. No model, tool, approval, extension, skill or rule flags were substituted, and existing host model authentication/configuration remained in effect. The `-race` isolated real-agent/board run passed in 21.48 seconds, wrote the exact proof, performed the scoped handoff, settled and returned positive cumulative token totals. This default-compatibility result is distinct from the restricted/yolo test profile above.

The combined **native standalone release + actual installed agent + authenticated model** scenario also passed. Set `SYMPHONY_ACTUAL_OMP_BINARY` to an absolute freshly built `go build ./cmd/symphony` executable while enabling the opt-in tests. This requires installed `cloudflared` and network access: a short-lived quick tunnel exposes only a synthetic loopback GraphQL fixture protected by a fresh 256-bit synthetic token. The unmodified macOS release binary uses publicly trusted HTTPS; model credentials stay local, no real issue data crosses the tunnel, and no OS trust or production TLS setting changes. The real model writes its proof, updates board Status through the registered host tool, settles, exits its worker and shuts down cleanly on SIGTERM. `after_run` archives proof outside the issue workspace before required terminal cleanup; the test never assumes uncommitted artifacts survive Done. The default 30-second polling cadence permits settled-success proof; rapid terminal reconciliation legitimately cancels an agent still composing its final response and is a separate runtime scenario.

Repeated real-agent evidence: the complete actual-agent set (native handoff, default/always-ask transcript checks and pending-host-call cancellation) passed with `-race -count=2`. The final native lifecycle check was then repeated separately with `-race -count=2`, passing in 17.75 and 92.17 seconds, including structured completed-worker evidence, archived proof, observed terminal-workspace removal and clean SIGTERM. Network/model-dependent tests remain opt-in; offline fixtures stay deterministic.

After the core source freeze and repeated standalone Linux suite, a **freshly rebuilt native release** also passed the full pipeline with the **exact shipped** `omp --mode rpc --no-ui` command and `read_timeout_ms: 5000`: `-race -count=1`, 30.69 seconds. This final run required structured settled-turn/completed-worker evidence, the archived model proof, actual terminal workspace removal and clean SIGTERM, not merely a successful tool acknowledgement.

Quick-tunnel DNS advertisement can precede public record availability. The opt-in fixture now waits for A-record publication through [Google Public DNS's HTTPS JSON API](https://developers.google.com/speed/public-dns/docs/doh/json) **before its first native hostname lookup**, then admits the release only after ordinary native HTTPS succeeds. Only the synthetic tunnel hostname is queried; no resolver override, DNS flush, IP pinning or certificate bypass is used. An earlier HTTP2 connector attempt registered successfully but still failed hostname resolution before launching Symphony, so [HTTP2 transport selection](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/run-parameters/#protocol) is not reported as a DNS or production-service fix.

The existing `TestSpecCLIMainTLSProcessSmoke` builds a command-package test executable and invokes the real CLI `main()` in a subprocess with a test-only trusted fixture CA; production TLS defaults and OS trust remain unchanged. It verifies failure/retry, prompt reload, scoped Status transition, correlated host-tool result, cumulative statistics, tracker-secret removal, physical workspace cwd, terminal cleanup and graceful SIGTERM. Standalone release-binary and expanded runtime scenario evidence is maintained in [end-to-end validation](e2e-validation.md). The test-main fixture alone is not standalone release/native-certificate proof.

Sources: [GitHub Projects API guide](https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/using-the-api-to-manage-projects), [Projects GraphQL reference](https://docs.github.com/en/graphql/reference/projects), [Issues GraphQL reference](https://docs.github.com/en/graphql/reference/issues), and repository `SPEC.md` §§10.5, 11, 15.3.
