# leakscan

`leakscan` is the repository-hygiene gate used by `make ci`. It reads the
working tree paths returned by `git ls-files -z`, so untracked files are not
part of the scan. Files containing NUL bytes, files larger than 2 MiB, and
non-regular entries are fail-closed findings in the `skip` category (reason
`binary`, `large`, or `other`), not silent successes. A path-only
`skip <path-glob> # reason` rule is required to allow an intentionally skipped
file. Binary files are still searched for detector matches when their bytes
can be read. The tool is an independent Go `main` package and uses only the
standard library.

The built-in categories are:

- `secrets`: common cloud, API, source-control, chat, PEM private-key, and JWT
  token shapes.
- `private_ip`: RFC 1918 and CGNAT IPv4 literals.
- `personal_path`: user home paths on macOS, Linux, and Windows.
- `internal_id`: gate IDs and CI run URLs.
- `hosts`: Tailscale tailnet domains matching `[a-z0-9-]+\.ts\.net\b` (case-insensitive).
- `private`: patterns supplied through the private list described below.
- `skip`: unreadable, oversized, binary, or non-regular tracked paths that
  require a path-only allow rule.

Each finding is reported as `path:line: category: <masked-match>`. Only the
first four characters of a built-in match are shown. Private-list findings
show only the pattern number; neither the private expression nor its match is
printed. The summary reports violations and allowed hits by category. A
violation exits 1; configuration or repository errors exit 2.

## Allow list

`tools/leakscan/allow.txt` is public and has one rule per line:

```
<category> <path-glob> <exact-match-or-re:regex> # reason
```

The reason is mandatory. Blank lines and comments are allowed. An exact
matcher is compared to the complete detected match; a matcher prefixed with
`re:` is matched as a complete regular expression. Path globs use `/` as the
separator and support `*`, `?`, character classes, and recursive `**`.

Every matching allow rule is counted. Unused rules produce a warning so stale
exceptions are visible; pass `-strict-allow` to make an unused rule fail the
scan. Keep exceptions narrow (a test path and a test value), and do not put
private operator identifiers in this public file.

## Private patterns

Set `LEAKSCAN_PRIVATE_PATTERNS` to a file outside the repository. Each
non-empty, non-comment line is a Go regular expression and is loaded as
category `private`. Blank lines and lines whose first non-space character is
`#` are ignored. Expressions that match the empty string (for example `x*`)
are ignored because they would report every file. Read and validation errors
do not disclose the private pattern file path or expression.
The file is never committed, and its expressions and matches are never
printed. For example:

```sh
LEAKSCAN_PRIVATE_PATTERNS="$HOME/private-patterns.txt" make leakscan
```

The path may be absolute or relative to the process working directory. Keep
this file access-controlled because the expressions can themselves be
sensitive.

## When the scan fails

First remove or redact the value from the tracked file and rerun the scan.
Only when the value is an intentional, non-sensitive fixture should a narrow
allow rule be added, with a reason and the smallest useful path glob. Never
use an allow rule to retain a real credential or private operator data.

Run `make leakscan` locally; the same target runs as part of `make ci` (and is
therefore included by `ci-linux`).

Tailnet IPv6 (`fd7a:115c:a1e0::/48`) and other ULA addresses are private
patterns when they are organization-specific; add them through the private
pattern file rather than publishing an operator-specific allow rule. File
names themselves are not scanned: only the contents of tracked files and the
contents of readable symlink targets are inspected.
