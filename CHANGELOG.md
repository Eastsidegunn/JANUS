# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/) from its first tagged release, v0.1.0. Until 1.0, minor versions may contain breaking changes.

Entries are grouped by implementation task (T0–T33). Requirement IDs refer to the [functional spec](docs/hx-기능명세서-v0.1.md); test evidence is in [docs/traceability.md](docs/traceability.md).

## [Unreleased]

## [0.1.2] - 2026-10-08

Security release: the approval gate now detects a bypassed PreToolUse hook (T33) and the agent is started with `--strict-mcp-config`. No interface or schema changes.

### Fixed

- **Claude approval-gate bypass detection (FR-POL-05, FR-ADP-10).** In `tool_approval` sessions, the Claude adapter now ends the session with an error after observing a non-rejected tool result whose call ID has no preceding approval request. The result is still recorded before termination so the append-only log reflects that the tool may already have run once.
- **Claude pre-hook input-validation results (T33 B1).** An unapproved result is exempt from the detective fatal only for the exact Claude-authored `<tool_use_error>...</tool_use_error>` whole-content shape with `is_error=true` (or a one-text-block array containing it). Other errors and successful results remain fatal, duplicate call IDs remain parser contract errors, stderr records each exemption, and the existing tool-result `raw` makes the call IDs recomputable from session logs without a schema change. Ordering and wrapper shape were measured six times with claude-code 2.1.293 (post-hook execution errors such as EISDIR/EACCES ran the hook and were not wrapped); pinned 2.1.252 remains an explicit smoke-test assumption.

### Security

- Host and world Claude paths share the same approval-decision-send ledger and stop the native process after a detected bypass. Parser-synthesized `permission_denied`/`user-rejected` non-execution results and the exact pre-hook input-validation shape remain valid without broad tool-name or generic-error exceptions.
- Claude Code starts with `--strict-mcp-config` and no `--mcp-config`, preventing workspace or user-configured MCP servers from forging approval-gate exemption output.

## [0.1.1] - 2026-10-08

Maintenance release: two fixes found on a real deployment run. No interface or schema changes.

### Fixed

- **Approval relay startup race (FR-POL-05, FR-CLI-06).** `hx run` now serializes stale-socket inspection, removal, and bind with an endpoint lock; it rejects live or non-socket owners before claim and treats the relay as ready only after `Listen` has bound successfully.
- Approval endpoint locking uses a permanent `<endpoint>.lock` file with `flock`; the lock file is deliberately not removed on shutdown.
- **Signal-safe production shutdown (FR-CLI-06, FR-SBX-01, FR-LOG-02).** A first SIGINT/SIGTERM reaches the adapter only through the graceful stop lifecycle. A second signal escalates that stop by killing only the agent container while keeping the broker wire alive, preserving the adapter-authored durable `done{stopped}` and filesystem collection before exit code 128+signal. The kill is immediate when Podman permits `podman kill` on a stopping container (verified in CI with Podman 5.x); otherwise Podman's 10-second stop grace completes with SIGKILL. A third signal is ignored while shutdown waits for the 30-second finalization fallback. If the lifecycle still has not completed after 30 seconds, the final fallback closes the lease and exits immediately; terminal evidence may be absent only in that fallback.
- Terminal control messages include the optional `done.reason` field for lifecycle-consumed signals and omit it when empty.

## [0.1.0] - 2026-10-08

First public release, corresponding to functional spec v0.1. Pre-1.0: interfaces, schemas and CLI flags may still change. See the README for platform requirements and known limitations.

### Added

- **Session log and replay.** Append-only SQLite event log with a single writer, WAL and full-sync durability, crash recovery, backpressure and redaction before write. Deterministic replay of derived state, and fork at a sequence number into a new trace with the original left unchanged (FR-LOG).
- **Contracts and codegen.** JSON Schemas for events and the adapter wire protocol, with Go types generated from them and a drift check in CI.
- **Loop and hooks.** Fixed turn/step state machine with four hook points and a reject > rewrite > continue verdict model, with verdicts recorded in the log (FR-LOOP).
- **Policy.** YAML profile parser (strict, duplicate-key and multi-document rejection), pure evaluation function and merge that can only narrow (allow-list intersection, budget minimum), verified by property tests (FR-POL).
- **CLI.** `hx run`, `hx replay`, `hx audit`, `hx audit-accept`, `hx stop`, `hx dump-config`. NDJSON events on stdout, diagnostics on stderr (FR-CLI).
- **Sub-agent adapters.** Adapter wire protocol over stdin/stdout NDJSON, a null adapter for tests, a Claude Code adapter (stream-json normalisation with raw passthrough, usage reporting, approval escalation) and a Codex adapter executable. Adapters are validated against 15 recorded golden fixtures (FR-ADP).
- **Sandboxed execution.** Local world backend that runs the agent in a rootless Podman container with a workspace overlay, default-deny egress through an allowlist proxy, and the agent process isolated from the host-side adapter via a process broker (FR-SBX, FR-ADP-10). Declared gateway address pins (`egress_pins`) for gateways on private networks.
- **Effect plane.** Collector observing file changes in the overlay and egress decisions, joined to the log by span id (FR-COL).
- **Audit.** `hx audit` compares reported intent against observed effects and classifies each item as matched, reported-but-not-observed or observed-but-not-reported, with span and cost queries. `hx audit-accept` reconciles the acceptance registry against session logs (FR-AUD).
- **Extension passthrough.** Declared agent extensions are installed during a provisioning phase with hash pinning and a content-addressed cache; the installed set is recorded and the execution phase cannot inherit provisioning network access (FR-EXT).
- **Observability.** OpenTelemetry export of traces and spans (verified against a real Jaeger in CI) and a deterministic, secret-redacting `hx dump-config` (FR-OBS).
- **Production run path.** `hx run` assembles the Podman world from a request, profile and overlays with scoped idempotency keys, a durable acceptance record written before any external effect, and tombstones that prevent reusing a deleted session key. Policy files that changed after being pinned (e.g. via `hx dump-config`) are rejected at run time (`POLICY_CHANGED`).
- **Remote approval and stop.** Approval decisions can be relayed over a local Unix socket with durable request and response, peer verification and deadline-based deny; `hx stop` requests an orderly stop through the owning process (FR-POL-05, FR-POL-06).
- **Multi-turn sessions.** Opt-in multi-turn mode for the Claude Code adapter: follow-up user messages are injected through the existing control socket and recorded with consecutive sequence numbers; later tool uses still go through approval.
- **Backoff warning.** `hx audit` and `hx replay` surface a one-line warning when a session shows no model-visible events after ready and repeated egress denies to the same domain. Observation only; it never terminates a session.
- **CI gates.** Linux integration gates against real rootless Podman, repeated five times on the same commit for the Claude-container path (`t15`) and the container tool-use approval path (`t27`). The t15 gate runs the fixed-version real Claude Code CLI (2.1.252) image without credentials to establish the unauthenticated failure path and credential non-leakage; t27 uses a fake Claude binary to drive both hook orders.

- **Configurable runtime socket directory (FR-CLI-08).** `HX_RUNTIME_DIR` selects the existing absolute parent for runtime sockets and temporary broker directories; unset defaults to `/tmp` and ignores `TMPDIR`, with platform socket-path budget validation before session start.
- **T29 log redaction coverage (FR-LOG-08, FR-SBX-04).** Production runs now use one redactor for literal adapter secrets, configured patterns, event payload/raw, control output, and adapter diagnostics; Codex and approval-request paths are covered by production-writer tests.

### Changed

- World-config `secret_env`/heuristic secret values shorter than eight bytes, numeric, or boolean are rejected before claim. Literal values include JSON HTML-escaped/unescaped forms, and host mode remains explicitly out of scope.
- Subscription authentication is no longer handled by JANUS. The container agent talks to a standard API endpoint provided by an operator-run gateway, and the egress proxy permits only that destination. The earlier proxy-held-credential design was removed.
- **FR-SBX-04 status.** FR-SBX-04 was revised (2026-10-06) to the current threat model: containers never receive vendor credentials; the only credential is an operator-gateway access key whose reachable destinations are limited by the egress allowlist/pins. Short-lived, session-scoped gateway tokens will be revisited if external use grows.
  T29 extends exact-value masking to container production streams, event payload/raw, and control output for built-in, `secret_env`, and `_KEY`/`_TOKEN`/`_SECRET`/`_PASSWORD` names; short/generic values are rejected and `redaction_patterns` is supported. Host mode remains a development-path limitation.
- The Claude Code agent process runs inside the container with the same argument vector the host-mode adapter uses, instead of a separate task-injection path.
- Synthetic API-error messages from Claude Code (for example "Request timed out") are no longer recorded as model text. A terminal one becomes the reason of a `done` with error status, and the raw output is preserved.

### Fixed

- **Approval hook fail-closed (FR-POL-05, FR-ADP-10).** Hook helper errors now exit 2, the helper deadline is shorter than Claude's hook timeout, and host approval uses one 480-second window fixed at session start (requests after it are denied; per-request caps remain a follow-up). Fakeclaude exercises Claude's exit-2-only blocking semantics.
- Process broker could report a container exit before the container had started (about half of runs under a race), closing the adapter early and failing sessions with tool calls.
- A hang when closing the approval client if its poller had just connected.
- Deadlock in the hook-first ordering used by real Claude Code (the hook runs before the assistant line is printed), which stalled every sandboxed tool call (verified in CI with a fake Claude binary emulating hook-first order; real-Claude rerun pending).
- Race in host mode where a stop arriving just after the result was replayed produced `ok` instead of `stopped`.
- Intermittent orphan-lifecycle failure in the container lifecycle tests (root-caused rather than re-run).

### Security

- Containers receive no vendor subscription tokens from JANUS. The only credential in the container environment is the operator-run gateway's access key, which is kept out of argv, logs and metadata; it is visible in the container's environment (e.g. `podman inspect`) to the host user.
- Egress denied by default; every allow and deny is logged by the collector.
- Policy merge cannot widen permissions or budgets (property-tested).
- Log writes cannot be updated or deleted at the storage level (triggers) or through the API.

[Unreleased]: https://github.com/Eastsidegunn/JANUS/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/Eastsidegunn/JANUS/releases/tag/v0.1.2
[0.1.1]: https://github.com/Eastsidegunn/JANUS/releases/tag/v0.1.1
[0.1.0]: https://github.com/Eastsidegunn/JANUS/releases/tag/v0.1.0
