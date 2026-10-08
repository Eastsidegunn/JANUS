# Security Policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Report privately via GitHub: **Security** tab → **Report a vulnerability** ([private vulnerability reporting](https://github.com/Eastsidegunn/JANUS/security/advisories/new)). Please do not open a public issue with exploit details. If GitHub private reporting is unavailable, open an issue titled **Security contact request** without details (or contact the maintainer via their GitHub profile), and we will arrange a private channel.

Include what you observed, how to reproduce it, the commit you tested and your environment (OS, Podman version). This is a pre-1.0 project maintained by one person, so there is no response-time guarantee, but reports are taken seriously.

## Supported versions

JANUS is pre-1.0. Security fixes land on `main` and in the latest v0.1.x release; older releases are not maintained.

## Threat model (summary)

JANUS runs coding agents that you do not trust. The design assumes:

- **The agent is untrusted.** It may misbehave, be prompt-injected, or try to reach things it should not.
- **Approval hooks are cooperative.** The protocol-level approval path (tool-use hooks relayed to a host-side decider) depends on the agent honouring the hook. It is a control, not a security boundary.
- Approval hook fail-closed scope: `hxapprove` errors and its 540-second
  deadline return exit 2 and block the tool call. Claude's own 600-second hook
  timeout is deliberately non-blocking; `hxapprove` is bounded below it and
  gives up first. The agent image must provide `/bin/sh` and `hxapprove` on
  `PATH`; if the hook cannot start, Claude proceeds (fail-open by the host
  tool's documented semantics). As a detective check, a tool result without a
  preceding approval decision send ends the session with an error; the
  tool may already have run once. Detection depends on Claude emitting a
  `tool_result`; if an already-executed tool ends Claude first, the session ends
  with an abnormal-exit error. If a decision is sent but `hxapprove` stalls
  before writing it to stdout and Claude later times out the hook, this detector
  cannot distinguish that failure from a delivered decision.
- A narrowly measured pre-hook exception applies only when an unapproved
  `tool_result` has `is_error=true` and its entire trimmed text content is
  wrapped by `<tool_use_error>...</tool_use_error>`. Array content must contain
  exactly one text block with that shape; partial matches, mixed/multiple
  blocks, plain execution errors, successful results, and duplicate results
  remain fatal. The adapter writes an exemption diagnostic to stderr, while
  the normalized result's existing `raw` field preserves the native shape and
  call ID for recomputation from the session log. This ordering and wrapper
  were measured six times with claude-code 2.1.293: a validation failure
  (Edit with an unmatched `old_string`) returned the wrapper without running
  the hook, while normal results and execution errors (missing-file Read,
  non-zero Bash exit, Read of a directory, Write into a read-only directory)
  all ran the hook and came back as plain text, so the wrapper cannot shield
  a tool that actually executed (short of stdout forgery). The fixed
  container version 2.1.252 has not yet been measured and is an explicit
  assumption covered by the operator-only `smoke` build-tag test.
- **OS-level isolation is the backstop.** The agent runs in a rootless Podman container. Network egress is denied by default and goes through an egress proxy that enforces a domain allowlist and records every allow and deny. File changes land in an overlay and are observed from outside the agent. These do not depend on the agent's cooperation.
- **The log is evidence.** The event log is append-only through a single writer, and the effect plane (what the sandbox observed) can be compared against what the agent reported (`hx audit`).
- **Policy only narrows.** Merging policy profiles can only reduce permissions and budgets.

Out of scope or known limits:

- Isolation is only as strong as rootless Podman, the container runtime and the Linux kernel underneath it. JANUS does not defend against a container-escape vulnerability in those components.
- The egress allowlist is by destination domain. It does not inspect the content of permitted traffic.
- Sandboxed execution is supported on Linux with rootless Podman only; macOS is for development and unit tests. The macOS Podman VM is not treated as representative of a deployment kernel.
- The t15 CI gate runs the fixed-version real Claude Code CLI (2.1.252) image without credentials and establishes the unauthenticated failure path and credential non-leakage. The t27 CI gate uses a fake Claude binary to drive the container tool-use approval path in both hook orders. With real tokens, the maintainer has manually checked only a tool-free, multi-turn session inside the container; real Claude container tool-use execution is not yet verified.
- FR-SBX-04 was revised (2026-10-06) to the current threat model: containers never receive vendor credentials; the only credential is an operator-gateway access key whose reachable destinations are limited by the egress allowlist/pins. Short-lived, session-scoped gateway tokens will be revisited if external use grows.
- A declared `egress_pins` domain can dial a private address; `isPublicIP` is not applied to that domain. Operators must declare it intentionally, and the effective destination is still the intersection with the policy allowlist.
- In the container production path, exact values from built-in, `secret_env`, and `_KEY`/`_TOKEN`/`_SECRET`/`_PASSWORD` environment names are masked in streams, event payload/raw, and control output; short/generic values are rejected during config parsing, and `redaction_patterns` extends the patterns. Operators remain responsible for identifying vendor keys.
- Line-oriented stdout/stderr redaction does not guarantee detection of a multi-line PEM value; event payload redaction uses the full pattern.
- A PEM credential that crosses the 64 KiB line-writer boundary is subject to the same line-oriented limitation; event payload redaction still uses the full pattern.
- Host mode (`hx run --session --adapter`, no container) is a development path: only the default credential regexes are applied to its event log, and adapter stdout/stderr is not masked.
- The gateway access key is visible in host `podman inspect` (container Config.Env) by design, and it can be sent to any allowlisted domain (no content inspection). Whether a world-config env value is a gateway key or a vendor key is the operator's responsibility; JANUS only rejects the subscription OAuth env name.
- Runtime Unix sockets are placed below `HX_RUNTIME_DIR` when set (otherwise `/tmp`). The directory must already exist, be absolute, and be short enough for the platform's Unix socket limit. Shared-writable directories require the sticky bit; mode `0700` is recommended so unrelated local users cannot access runtime capabilities. At startup, a refused stale approval socket is removed; a live listener or a non-socket object at that path is rejected before the session claim.
- A SIGINT/SIGTERM received during claim, before the session writer is open, retains the process default and may terminate immediately after consuming the claim.
- After the first signal, if the adapter ignores the graceful stop request, shutdown waits for a second signal; that wait is not bounded. A second signal kills only the agent container while leaving the broker wire and collectors alive so `done{stopped}` and filesystem evidence can be durably finalized before exit code 128+signal. This escalation is immediate when Podman permits `podman kill` on a stopping container (verified in CI with Podman 5.x); otherwise it completes after Podman's 10-second stop grace sends SIGKILL. A third signal is ignored, and shutdown continues waiting for the 30-second finalization fallback. If finalization does not finish within 30 seconds, JANUS uses a last-resort lease close and immediate exit; terminal records may be absent in that fallback case.

## Secrets

- JANUS does **not** handle vendor subscription tokens. Subscription authentication is externalised to a gateway the operator runs (for example CLIProxyAPI); the agent container talks to a standard API endpoint and the egress proxy permits only that destination.
- The only secret that the world-config may carry is the access key for that operator-run gateway, passed to the container environment in plain text. It is kept out of argv, logs and metadata, but is visible in the container's environment (e.g. `podman inspect`) to the host user. Treat the world-config file accordingly and do not commit it.
- Never put real tokens, keys or credentials in an issue, a pull request, a log or a fixture. Recorded fixtures are checked for secrets by `make fixtures`.
- This repository has GitHub secret scanning and push protection enabled.

If you accidentally expose a credential in this repository, revoke it with its issuer immediately and then report it as above.
