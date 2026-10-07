# JANUS (HX)

**JANUS is a multi-agent execution substrate: it runs coding agents (Claude Code today; a Codex adapter exists) inside rootless Podman containers under a narrowing policy, and records everything that happens in an append-only event log you can replay and audit.** The codebase and CLI use the codename **HX** (`hx run`, `hx replay`, ...). The Codex container path is currently limited to `container_only` mode and unit tests.

> **Status: pre-1.0 (v0.1.0).** Interfaces, event schemas and CLI flags can still change. Read [Status and limitations](#status-and-limitations) before relying on it.

## Why

Coding agents are useful, but you cannot take their word for what they did. JANUS is built on the opposite assumption: the agent is untrusted, and you should still be able to (1) bound what it can do, (2) reconstruct exactly what it did, and (3) attribute every action to a trace.

The goals, from the [functional spec](docs/hx-기능명세서-v0.1.md) (Korean):

- Orchestrate heterogeneous agents (external ones like Claude Code and Codex) in one session. The specification has this as a goal; the current `hx run` path launches one sub-agent per session.
- Attribute every action to a single trace (OpenTelemetry trace/span model; a sub-agent run is a child span).
- Isolate agents at the process boundary.
- Make every session reconstructable and verifiable after the fact.

## Core ideas

**Two independent control axes.**
1. *Protocol-level approval.* Tool calls pass through an approval hook. In the Claude Code path this is a `hxapprove` hook that relays to a host-side decider; an unanswered request is denied at its deadline and recorded durably, and a hook/intent mismatch fails the session. This path is cooperative: it relies on the agent honouring the hook. Codex runs in `container_only` mode with no per-tool approval.
2. *OS-level isolation as the backstop.* The agent runs in a rootless OCI container with egress denied by default (traffic goes through a proxy that enforces a domain allowlist and logs allow/deny decisions) and a workspace overlay whose changes are captured separately. This holds whether or not the agent cooperates.

**Two evidence planes.** The *intent plane* is what the agent/adapter reports. The *effect plane* is what the sandbox boundary observes without the agent's cooperation (file changes in the overlay, egress decisions). `hx audit` compares them and classifies each item as matched, reported-but-not-observed, or observed-but-not-reported.

**Append-only log, replay, fork.** All session events go through a single writer into a SQLite log with triggers that block `UPDATE`/`DELETE`. Derived state (model-visible history, usage, projections) is recomputed from the log, so replay is deterministic. A session can be forked at a sequence number into a new trace; the original is left untouched.

**Policy only narrows.** Policy profiles (YAML) are merged by intersecting allow-lists and taking the minimum of budgets. An overlay can tighten a profile but never widen it. This is checked with property tests.

**Credentials stay out of JANUS.** JANUS does not handle vendor subscription tokens. The containerised agent talks to a standard API endpoint that the operator provides (for example a gateway such as CLIProxyAPI), and the egress proxy only permits that destination.

## Architecture

Dependencies point one way:

```
contracts  <-  core  <-  seams  <-  surfaces
                          collector (effect plane; shares no code path with core)
```

| Layer | Directory | Contents |
|---|---|---|
| contracts | `contracts/` | JSON Schemas for events and the adapter wire protocol; Go types are generated from them |
| core | `core/` | session log writer (`logd`), loop, policy engine, audit, OTel projection |
| seams | `seams/` | replaceable implementations: SQLite store, local Podman world backend, sub-agent adapters (Claude Code, Codex, a null test adapter), approval relay, accept registry |
| surfaces | `surfaces/hx/` | the `hx` CLI, where everything is assembled |
| collector | `collector/` | filesystem-diff and egress observation, joined to the log only by span id |

Seams may not import each other horizontally; `make lint` runs a boundary linter (`tools/boundarylint`) that fails the build on layering violations.

CLI subcommands: `hx run`, `hx stop`, `hx replay`, `hx audit`, `hx audit-accept`, `hx dump-config`. Event output is NDJSON on stdout and diagnostics go to stderr, so it composes in pipelines.

## Status and limitations

- **Pre-1.0.** The functional spec is v0.1. Tasks T0–T30 have landed with their CI acceptance tests green; several manual, real-credential checks remain open (see [ROADMAP.md](ROADMAP.md), [docs/traceability.md](docs/traceability.md)). The first tagged release is v0.1.0; there is no stability promise before 1.0.
- **Where it runs.** Sandboxed execution is **Linux only**, with **rootless Podman and native overlayfs** (checked at startup; fuse-overlayfs is rejected). Podman ≥4.0 is needed for multi-network attach; it is tested on Podman 5.8 (CI) and 4.9.3 with netavark.
- **macOS is for development and unit tests only.** `make ci` runs there; the container integration targets (`world-integration`, `t15-integration`, ...) are Linux-only and refuse to run elsewhere rather than skip. In the maintainer's setup the macOS Podman VM did not share the needed host paths, and it is not a representative kernel.
- **Verification status.** The t15 CI gate runs the fixed-version real Claude Code CLI (2.1.252) image without credentials and establishes the unauthenticated failure path and credential non-leakage. The t27 CI gate uses a fake Claude binary to drive the container tool-use approval path in both hook orders. With real tokens, the maintainer has manually checked only a tool-free, multi-turn session inside the container; real Claude container tool-use execution is not yet verified. Codex passed a real CLI smoke in T16-3 in **host mode (outside the container)**; running Codex inside the container is still open ([traceability, T16-3 row](docs/traceability.md)).
- **FR-SBX-04.** FR-SBX-04 was revised (2026-10-06) to the current threat model: containers never receive vendor credentials; the only credential is an operator-gateway access key whose reachable destinations are limited by the egress allowlist/pins. Short-lived, session-scoped gateway tokens will be revisited if external use grows. See [docs/v0.1-release-acceptance.md](docs/v0.1-release-acceptance.md) and [docs/spec-change-proposals.md](docs/spec-change-proposals.md) (SCP-SBX04-001).
  In the container production path, exact values from built-in, `secret_env`, and `_KEY`/`_TOKEN`/`_SECRET`/`_PASSWORD` environment names are masked in streams, event payload/raw, and control output. Short/generic values are rejected during config parsing, and `redaction_patterns` extends the patterns; operators remain responsible for identifying vendor keys. Host mode is a development path and is limited to the default event-log regexes without stdout/stderr masking (see [SECURITY.md](SECURITY.md)).
- **Non-goals** (spec section 1.3): no web UI, no policy rule DSL, no JANUS-run plugin marketplace, no distributed control plane, no own LLM inference, no swapping of the agent loop itself.
- Container images for the egress proxy and the agent are referenced by sha256 digest in a world-config file that the operator supplies. This repository does not yet publish prebuilt images or binaries.

## Quick start

You need Go (the version in [go.mod](go.mod)), `make`, and a C toolchain for `-race`. The steps below need no credentials and no Podman. CLI diagnostics are currently in Korean.

```sh
git clone https://github.com/Eastsidegunn/JANUS.git
cd JANUS

make ci          # lint (incl. layering lint), tests with -race, CGO-free smoke, fixture checks, codegen drift
```

Run a token-free session with the test-only null adapter, then replay it:

```sh
rm -f /tmp/demo.db
go build -o /tmp/hx ./surfaces/hx
go build -o /tmp/nulladapter ./seams/subagent/nulladapter

/tmp/hx run --session /tmp/demo.db --adapter /tmp/nulladapter "hello"
/tmp/hx replay --session /tmp/demo.db
```

This is the "skeleton" path: no container sandbox, it only demonstrates the log, adapter wire protocol and replay. The production path (`hx run --request ... --profile ... --accept-root ... --world-config ...`) assembles the Podman world and requires Linux, Podman and an operator-provided world-config; the smoke runbooks in `docs/` describe it but are written for the maintainer's own environment and are not a polished setup guide yet.

On Linux with rootless Podman, `make ci-linux` additionally runs the container integration gates. `make t15-integration` and `make t27-integration` run the Claude-container and tool-use approval gates.

## Configuration

The production path requires a world config, a policy profile, and a run request. Start with the generic, non-secret templates in [`examples/`](examples/): [`world-config.example.json`](examples/world-config.example.json), [`policy-profile.example.yaml`](examples/policy-profile.example.yaml), and [`run-request.example.json`](examples/run-request.example.json). Replace every documented placeholder. Compute `profile_hash` as `sha256(uvarint(len(b0))‖b0‖uvarint(len(b1))‖b1…)`, where `b_i` are the profile then overlay files' raw bytes in order; follow the verified procedure in [docs/t17-20-smoke-runbook.md §profile-hash](docs/t17-20-smoke-runbook.md#profile-hash-계산). This differs from the `policy_hash` printed by `dump-config`, which hashes the rendered merged policy. Use a digest-pinned image (tags are rejected). The host must be Linux with rootless Podman and native overlayfs. Use `hx dump-config --profile … --workspace …` to inspect the merged policy, then run with `hx run --request … --profile … --accept-root … --world-config …`.

Runtime Unix sockets and other short-lived runtime directories use `HX_RUNTIME_DIR` when it is set. It must be an existing absolute directory: paths containing `:` are rejected, shared world-writable directories without the sticky bit are rejected, the directory must be writable and searchable, and it must leave room for the longest socket path on the host platform; otherwise the session is rejected before claim or `session/start`. When unset, the default is `/tmp` (the process intentionally ignores `TMPDIR` because Unix socket paths are shorter than ordinary temporary-file paths). Operators should use a private `0700` directory.

Supported adapters: `claudecode` and `codex` are compiled-in (see `surfaces/hx/run_request.go`); adding another adapter currently requires a code change.

## Documentation map

Most documents are in Korean. The spec and design records are the project's source of truth, and are kept in the language the maintainer works in.

| Read this | For |
|---|---|
| [docs/hx-기능명세서-v0.1.md](docs/hx-기능명세서-v0.1.md) | Functional spec (FR/NFR IDs): the authoritative description |
| [docs/traceability.md](docs/traceability.md) | Each requirement mapped to the tests that cover it |
| [docs/v0.1-release-acceptance.md](docs/v0.1-release-acceptance.md) | The spec's eight acceptance criteria against concrete tests and CI gates |
| [ROADMAP.md](ROADMAP.md) | What is still open after v0.1.0 |
| `docs/t*-proposal.md`, `docs/scp-*.md` | Design proposals per task and spec-change proposals |
| `docs/*smoke-runbook.md` | Manual, credentialed smoke procedures (maintainer-oriented) |
| [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), [CHANGELOG.md](CHANGELOG.md) | Contributing, security policy, changes |

Some design documents refer to a sibling private project and to maintainer-internal review notation. Documentation uses "명세 소유자" (spec owner), while some schemas and code comments retain the shorthand `[H]` with the same meaning. They are kept as the historical record.

## License

[MIT](LICENSE)
