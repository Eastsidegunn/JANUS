# Contributing to JANUS (HX)

Thanks for your interest. The project is pre-1.0 and developed in the open; contributions are welcome, but the bar for merging is strict because the repository enforces a set of invariants (append-only log, single writer, narrowing-only policy, layering).

The functional spec is [docs/hx-기능명세서-v0.1.md](docs/hx-기능명세서-v0.1.md) (Korean). It is the single source of truth: every change must trace to an FR/NFR ID in it. [docs/traceability.md](docs/traceability.md) maps each ID to the tests that cover it, and [ROADMAP.md](ROADMAP.md) lists what is still open.

## Workflow

1. **Open an issue first** describing the problem and which FR/NFR ID(s) it concerns. Features not in the spec (and anything in the non-goals, spec section 1.3: web UI, policy rule DSL, own marketplace, ...) will not be accepted without a spec change.
2. **Branch** from `main`. Keep one PR to one piece of work; do not implement follow-up items ahead of time.
3. **Make `make ci` green locally** before opening a PR. It runs lint (including the layering lint), `-race` unit and property tests, a CGO-free smoke run, golden fixture checks and the codegen drift check. Go version: see `go.mod`.

   ```
   make lint        # includes the boundary lint; a failure means a layering violation
   make test        # unit + property tests
   make fixtures    # adapter golden fixture comparison
   make ci          # all of the above; required before a PR
   ```

4. **Open a PR.** Commit messages and the PR title/description must name the target FR ID(s), for example `feat(logd): FR-LOG-02 single-writer seq assignment`. If you change behaviour covered by [docs/traceability.md](docs/traceability.md), update the matching row.
5. **Definition of done:** the acceptance tests for the work are green in CI, including the Linux integration gates below, which are required checks. "Implemented" is not done.
6. **If you are blocked,** do not work around the problem. Open an issue (or comment on the existing one) describing what blocks you and why, and stop there.

## Required checks (11)

`main` is protected by a ruleset (`main-protection`) requiring these 11 status checks; the jobs are defined in [.github/workflows/ci.yml](.github/workflows/ci.yml):

| Check | What it is |
|---|---|
| `ci` (1) | `make ci-linux` on `ubuntu-latest`: `make ci` plus the Linux container gates for the world backend, extension provisioning and Jaeger OTel export |
| `t15-linux-gate / attempt 1..5` (5) | The credential-free Claude-in-container gate (`make t15-integration`), run five times against the same commit. Each attempt builds its images afresh, so a pass is attributable to that exact SHA and not to a cached image |
| `t27-linux-gate / attempt 1..5` (5) | The container tool-use approval gate (`make t27-integration`), also five attempts on the same commit, covering both hook orders |

The repeated attempts exist because these gates exercise real containers and timing-sensitive paths. A flaky failure is treated as a bug to root-cause, not something to re-run until green. If a gate fails on your PR and you believe it is unrelated, say so in the PR rather than re-running silently.

The gates need Linux and rootless Podman, so you will usually see them for the first time in CI. On a Linux machine with rootless Podman you can run them locally with `make ci-linux`, `make t15-integration` and `make t27-integration`.

## Invariants

A change that violates any of these will not be merged:

- **The log is append-only.** Do not add `UPDATE`/`DELETE` paths.
- **Single writer.** All log writes go through the one writer; no other code opens the database file directly.
- **Derived state is recomputable.** Everything derived (history, usage, projections) must be recomputable from the event log.
- **Policy merging only narrows.** Allow-lists are intersected and budgets take the minimum; an overlay can never widen a profile.
- **Dependency direction** is `contracts <- core <- seams <- surfaces`. No horizontal imports between seams. The collector shares no code path with core.
- **Contracts are the truth.** The JSON Schemas in `contracts/` are authoritative; Go types are codegen output and are never hand-edited (`make codegen-drift` enforces this).

## Prohibited

- Weakening, deleting or skipping a test to get green, including lowering property-test iteration counts.
- Implementing features that are not in the spec, or that the spec lists as non-goals (section 1.3).
- Adding a new external dependency without maintainer approval. Open an issue with the candidate and the reason first.
- Editing `contracts/` in a feature PR. If you believe a schema must change, do not implement it; propose a spec change (see below).
- Hand-editing recorded fixtures under `contracts/fixtures/`. They are snapshots of external tool output; they are re-recorded only with maintainer approval.
- Leaving mocks or stubs that pose as real implementations. A stub needs a `Null` or `Fake` name prefix and must live on a test-only path.
- Committing real credentials, tokens, or private infrastructure details (hostnames, addresses, internal paths) anywhere, including test data and docs.

Run `make leakscan` before submitting changes; it blocks tracked credentials and private infrastructure details.

## Verification order

Before claiming a change is complete, in this order:

1. Confirm `make ci` is fully green.
2. State in one sentence which invariant your change touches (put it in the PR description).
3. Confirm that a property test for that invariant actually covers the path you changed. If none does, strengthen the test first.

## Changing `contracts/` or `contracts/fixtures/`

- `contracts/` holds the JSON Schemas, the source of truth for event and wire formats.
- Generated code under `contracts/gen/` is the output of `make codegen` from those schemas and is **never hand-edited**. To change it, change the schema through a spec-change proposal and regenerate; `make codegen-drift` fails if the generated files and the schemas disagree. Schema changes are among the most expensive to reverse and require maintainer review.
- `contracts/fixtures/` holds recorded output of external tools (snapshots). Recordings need real credentials and are made by the maintainer.

If you think either needs to change (or the spec itself), do not implement it. Write a **spec-change proposal (SCP)** — what changes, why, which FR it affects, and compatibility impact — as an entry in [docs/spec-change-proposals.md](docs/spec-change-proposals.md), and open an issue pointing to it. The existing entries and the longer `docs/scp-*.md` files are examples. Implementation follows only after the maintainer accepts the proposal. `contracts/` (including `contracts/fixtures/`) and `.github/` are listed in CODEOWNERS, so the maintainer is requested for review.

## Security issues

Do not file vulnerabilities as public issues. See [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contribution is licensed under the [MIT License](LICENSE).
