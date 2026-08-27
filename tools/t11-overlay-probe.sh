#!/bin/sh
set -eu

root=$(mktemp -d)
cid=
trap 'if [ -n "$cid" ]; then podman rm -f "$cid" >/dev/null 2>&1 || true; fi; rm -rf "$root"' EXIT
lower=$root/lower
state=$root/state
upper=$state/upper
work=$state/work
mkdir -p "$lower/opaque" "$state/overlay"
mkdir "$upper" "$work"
printf 'base\n' > "$lower/modify.txt"
printf 'delete\n' > "$lower/delete-me.txt"
printf 'rename\n' > "$lower/rename-old.txt"
printf 'hard\n' > "$lower/hard-base.txt"
printf 'opaque-a\n' > "$lower/opaque/a.txt"
printf 'opaque-b\n' > "$lower/opaque/b.txt"
before=$(sha256sum "$lower/modify.txt" "$lower/delete-me.txt" "$lower/rename-old.txt" "$lower/hard-base.txt" "$lower/opaque/a.txt" "$lower/opaque/b.txt")

cat > "$root/probe.go" <<'EOF'
package main

import (
	"os"
	"path/filepath"
)

func main() {
	write := func(name, body string) { if err := os.WriteFile(filepath.Join("/workspace", name), []byte(body), 0o600); err != nil { panic(err) } }
	write("modify.txt", "modified\n")
	write("new.txt", "new\n")
	if err := os.Remove("/workspace/delete-me.txt"); err != nil { panic(err) }
	if err := os.Rename("/workspace/rename-old.txt", "/workspace/renamed.txt"); err != nil { panic(err) }
	if err := os.Symlink("modify.txt", "/workspace/link.txt"); err != nil { panic(err) }
	if err := os.Link("/workspace/hard-base.txt", "/workspace/hard-link.txt"); err != nil { panic(err) }
	write("hard-base.txt", "hard-modified\n")
	if err := os.RemoveAll("/workspace/opaque"); err != nil { panic(err) }
}
EOF
CGO_ENABLED=0 go build -trimpath -o "$root/probe" "$root/probe.go"
printf 'FROM scratch\nCOPY probe /probe\nENTRYPOINT ["/probe"]\n' > "$root/Containerfile"
podman build --pull=never -t localhost/t11-overlay-probe:latest -f "$root/Containerfile" "$root" >/dev/null
digest=$(podman image inspect --format '{{.Digest}}' localhost/t11-overlay-probe:latest)
case "$digest" in sha256:????????????????????????????????????????????????????????????????) ;; *) echo "bad digest: $digest" >&2; exit 1;; esac
cid=$(podman create --userns=keep-id --network=none -v "$lower:/workspace:O,upperdir=$upper,workdir=$work" localhost/t11-overlay-probe@"$digest")
podman start -a "$cid"
podman inspect --format 'digest={{index .ImageDigest 0}}' "$cid"
after=$(sha256sum "$lower/modify.txt" "$lower/delete-me.txt" "$lower/rename-old.txt" "$lower/hard-base.txt" "$lower/opaque/a.txt" "$lower/opaque/b.txt")
[ "$before" = "$after" ] || { echo 'lower changed' >&2; exit 1; }
printf '%s\n' '--- upper entries ---'
find "$upper" -mindepth 1 -print | sort
printf '%s\n' '--- upper stat ---'
find "$upper" -mindepth 1 -exec stat -c 'path=%n type=%F uid=%u gid=%g mode=%a inode=%i rdev=%t:%T' {} \; | sort
printf '%s\n' '--- upper xattrs ---'
if command -v getfattr >/dev/null 2>&1; then getfattr -R -d -m- "$upper" 2>&1 || true; else echo 'getfattr unavailable'; fi
