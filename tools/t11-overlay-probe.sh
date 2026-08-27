#!/bin/sh
set -eu

root=$(mktemp -d)
cid=
cleanup() {
  if [ -n "$cid" ]; then podman rm -f "$cid" >/dev/null 2>&1 || true; fi
  # Rootless overlay work entries may be subordinate-owned; unshare is the
  # probe's cleanup path so cleanup errors do not hide the captured result.
  podman unshare rm -rf -- "$root" >/dev/null 2>&1 || rm -rf -- "$root" || true
}
trap cleanup EXIT

cat > "$root/probe.go" <<'EOF'
package main

import (
	"os"
	"path/filepath"
)

func main() {
	mode := os.Getenv("MODE")
	join := func(name string) string { return filepath.Join("/workspace", name) }
	switch mode {
	case "recreate":
		if err := os.RemoveAll(join("dir")); err != nil { panic(err) }
		if err := os.Mkdir(join("dir"), 0o755); err != nil { panic(err) }
		if err := os.WriteFile(join("dir/new.txt"), []byte("new\n"), 0o600); err != nil { panic(err) }
	case "replace":
		if err := os.RemoveAll(join("dir")); err != nil { panic(err) }
		if err := os.WriteFile(join("dir"), []byte("replacement\n"), 0o600); err != nil { panic(err) }
	case "partial":
		if err := os.Remove(join("dir/a.txt")); err != nil { panic(err) }
	default:
		panic("unknown mode")
	}
}
EOF
CGO_ENABLED=0 go build -trimpath -o "$root/probe" "$root/probe.go"
printf 'FROM scratch\nCOPY probe /probe\nENTRYPOINT ["/probe"]\n' > "$root/Containerfile"
podman build --pull=never -t localhost/t11-overlay-probe:latest -f "$root/Containerfile" "$root" >/dev/null
digest=$(podman image inspect --format '{{.Digest}}' localhost/t11-overlay-probe:latest)
case "$digest" in sha256:????????????????????????????????????????????????????????????????) ;; *) echo "bad digest: $digest" >&2; exit 1;; esac

run_case() {
	mode=$1
	lower=$root/$mode/lower
	state=$root/$mode/state
	upper=$state/upper
	work=$state/work
	mkdir -p "$lower/dir" "$upper" "$work"
	printf '%s\n' "a-$mode" > "$lower/dir/a.txt"
	printf '%s\n' "b-$mode" > "$lower/dir/b.txt"
	if [ "$mode" = replace ]; then printf 'root-file\n' > "$lower/root.txt"; fi
	cid=$(podman create --userns=keep-id --network=none -e MODE="$mode" -v "$lower:/workspace:O,upperdir=$upper,workdir=$work" localhost/t11-overlay-probe@"$digest")
	podman start -a "$cid"
	podman rm "$cid" >/dev/null
	cid=
	echo "CASE=$mode"
	echo '--- upper entries ---'
	find "$upper" -mindepth 1 -print | sed "s#^$upper/##" | sort
	echo '--- upper stat ---'
	find "$upper" -mindepth 1 -exec stat -c 'path=%n type=%F uid=%u gid=%g mode=%a inode=%i rdev=%t:%T' {} \; | sort
	echo '--- upper xattrs ---'
	if command -v getfattr >/dev/null 2>&1; then getfattr -R -d -m- "$upper" 2>&1 || true; else echo 'getfattr unavailable'; fi
}

run_case recreate
run_case replace
run_case partial
