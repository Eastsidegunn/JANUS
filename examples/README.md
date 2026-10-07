# HX configuration examples

These files are parser-valid templates, not ready-to-run credentials or images:

- `world-config.example.json` describes the Podman state root, proxy image, Claude and Codex adapters, gateway environment, optional `secret_env`, and `redaction_patterns`. Replace repositories, all `sha256:` digests, non-root UID/GID values, binaries/argv, gateway URL, and the `REPLACE_WITH_*_GATEWAY_ACCESS_KEY` placeholders. Image digests must be fixed; image tags are rejected. `egress_pins` is intentionally omitted: add it only when you have a real domain/address pin that is also allowed by the merged policy. Secret values must be at least eight characters and must not be numeric or boolean; configure names explicitly with `secret_env` when they do not use the `_KEY`/`_TOKEN`/`_SECRET`/`_PASSWORD` suffixes. Container production masks these values in streams, event payload/raw, and control output (see [SECURITY.md](../SECURITY.md)). **Warning: Codex inside a container is not yet verified end to end.**
- `policy-profile.example.yaml` is the base policy. Replace the profile ID, absolute filesystem scope, gateway allowlist, and budgets for your deployment. `approval` is required (`manual` or `auto`).
- `run-request.example.json` is a v1 request. Replace every `REPLACE_WITH_*` value, use the adapter you configured, and calculate `profile_hash` from the exact profile file bytes (including any overlays) using `sha256(uvarint(len(b0))‖b0‖uvarint(len(b1))‖b1…)`, where `b_i` are the profile then overlay files' raw bytes in order. The verified calculation procedure is in [docs/t17-20-smoke-runbook.md §profile-hash](../docs/t17-20-smoke-runbook.md#profile-hash-계산). This is distinct from the `policy_hash` printed by `dump-config`, which hashes the rendered merged policy. The fingerprint and hash placeholders are format-valid lowercase SHA-256 strings, not real pins.

Create `state_root` in advance as a directory with exact mode 0700 (`seams/world/local/local.go` enforces this); an `accept-root` that does not exist is created with mode 0700. Change `/var/lib/janus` to a path owned by the rootless operator. Keep the workspace outside `state_root`, for example `/srv/janus/workspace` as used above.

The production path assumes Linux, rootless Podman, and native overlayfs. After editing a profile, inspect the merged result with:

```sh
hx dump-config --profile examples/policy-profile.example.yaml --workspace /srv/janus/workspace
```

Then submit a request (with an operator-owned accept root and world config):

```sh
hx run --request examples/run-request.example.json \
  --profile examples/policy-profile.example.yaml \
  --accept-root /var/lib/janus/accept \
  --world-config examples/world-config.example.json
```

Set `HX_RUNTIME_DIR` to an existing absolute directory when `/tmp` is unavailable. Paths containing `:` are rejected, shared world-writable directories without the sticky bit are rejected, the directory must be writable and searchable, and the path must stay within the Unix socket length limit; `/tmp` remains the default and `TMPDIR` is intentionally ignored. A private directory with mode `0700` is recommended.
