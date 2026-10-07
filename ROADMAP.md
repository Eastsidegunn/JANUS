# Roadmap

JANUS (HX) is pre-1.0. This page lists what is still open, at feature level. Requirement IDs refer to the [functional spec](docs/hx-기능명세서-v0.1.md); per-requirement test status is in [docs/traceability.md](docs/traceability.md), and the spec's v0.1 acceptance criteria are checked in [docs/v0.1-release-acceptance.md](docs/v0.1-release-acceptance.md). Nothing below is done unless it says so.

## Verification still open

The mechanisms below are covered by CI with fake or credential-free agents; what remains is an end-to-end check with real credentials.

- **Real-credential Claude tool use inside the container (FR-SBX-01, FR-ADP-10).** CI proves container start-up, stdio, overlay, egress, the approval relay and the tool-use approval path (with a fake Claude binary). With real tokens, only a tool-free, multi-turn session inside the container has been checked so far.
- **Codex inside the container (FR-SBX-01).** A real Codex session has passed in host mode (outside the container); running Codex inside the container is still open, and the Codex container path is currently limited to `container_only` mode.

## Later / under consideration

- **Short-lived, session-scoped gateway tokens (FR-SBX-04, SHOULD).** Today the container receives a static operator-gateway access key whose reachable destinations are limited by the egress allowlist. Short-lived, per-session tokens will be revisited if external use grows.
- **Published container images and binaries.** The egress proxy and agent images are currently built by the operator and referenced by digest; no prebuilt images or release binaries are published yet.
- **More adapters beyond Claude Code and Codex.** Adding an adapter currently needs a code change; further agents (including in-house agents, whose reference protocol is an open item in spec appendix A) are a test of how stable the contracts are.
- **Remote / microVM world backend (FR-SBX-05).** The spec requires that the world backend can be swapped (local → remote microVM) without consumer code changes. Only the local rootless Podman backend exists today.
- **Open items from spec appendix A** — exec auditing (eBPF), the criteria for moving to a Postgres store, raw-field compression and retention, and the default redaction rule set. These need a spec decision before any implementation.

Non-goals (spec section 1.3) are not on this roadmap: no web UI, no policy rule DSL, no JANUS-run plugin marketplace, no distributed control plane, no own LLM inference, no swapping of the agent loop itself.
