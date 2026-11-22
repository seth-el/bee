#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: scripts/install.sh --prefix /usr/local --archive dist/bee-VERSION-linux-ARCH.tar.gz [--dry-run]

Installs foreground Bee binaries under PREFIX/bin. This script never creates or
modifies ~/.bee and never invokes OpenClaw.
EOF
}

PREFIX=
ARCHIVE=
DRY_RUN=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --prefix) [[ $# -ge 2 ]] || { usage; exit 2; }; PREFIX=$2; shift 2 ;;
    --archive) [[ $# -ge 2 ]] || { usage; exit 2; }; ARCHIVE=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

[[ -n "$PREFIX" && "$PREFIX" = /* && "$PREFIX" != / ]] || { echo "--prefix must be an absolute non-root path" >&2; exit 2; }
[[ -n "$ARCHIVE" && -f "$ARCHIVE" ]] || { echo "--archive must name an existing archive" >&2; exit 2; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 2; }
command -v install >/dev/null 2>&1 || { echo "install is required" >&2; exit 2; }

# Reject absolute paths and parent traversal before extraction.
if tar -tzf "$ARCHIVE" | awk '/(^|\/)\.\.($|\/)|^\// { bad=1 } END { exit bad }'; then :; else
  echo "archive contains unsafe paths" >&2
  exit 1
fi

TMP=$(mktemp -d "${TMPDIR:-/tmp}/bee-install.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

tar -xzf "$ARCHIVE" --no-same-owner -C "$TMP"
BEE=$(find "$TMP" -type f -name bee -perm -u+x -print -quit)
[[ -n "$BEE" ]] || { echo "archive does not contain an executable bee" >&2; exit 1; }
TELEGRAM=$(find "$TMP" -type f -name bee-telegram -perm -u+x -print -quit || true)

if [[ "$DRY_RUN" == "1" ]]; then
  printf 'would install %s to %s/bin/bee\n' "$BEE" "$PREFIX"
  [[ -n "$TELEGRAM" ]] && printf 'would install %s to %s/bin/bee-telegram\n' "$TELEGRAM" "$PREFIX"
  exit 0
fi

mkdir -p "$PREFIX/bin"
install -m 0755 "$BEE" "$PREFIX/bin/bee"
if [[ -n "$TELEGRAM" ]]; then
  install -m 0755 "$TELEGRAM" "$PREFIX/bin/bee-telegram"
fi
printf 'installed Bee binaries under %s/bin\n' "$PREFIX"
