#!/usr/bin/env bash
#
# ocstats installer — downloads the single-file Python CLI (zero dependencies).
#
#   curl -fsSL https://raw.githubusercontent.com/veekayinhk/ocstats/main/install.sh | bash
#
# Options (flags or environment):
#   --bin-dir DIR | PREFIX=DIR   install directory (default ~/.local/bin)
#   --ref REF     | OCSTATS_REF=REF   git ref to install from (default main)
#   --with-bash   | WITH_BASH=1  also install ocstats.sh (needs sqlite3 + jq)
#   --help                       show this help

set -euo pipefail

REPO_RAW="https://raw.githubusercontent.com/veekayinhk/ocstats"
BIN_DIR="${PREFIX:-$HOME/.local/bin}"
REF="${OCSTATS_REF:-main}"
WITH_BASH="${WITH_BASH:-0}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --bin-dir) BIN_DIR="$2"; shift 2 ;;
        --ref)     REF="$2"; shift 2 ;;
        --with-bash) WITH_BASH=1; shift ;;
        -h|--help) sed -n '3,13p' "$0"; exit 0 ;;
        *) echo "install.sh: unknown option '$1' (see --help)" >&2; exit 1 ;;
    esac
done

say()  { printf '%s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# --- prerequisites -----------------------------------------------------------

command -v python3 >/dev/null 2>&1 || fail "python3 not found — ocstats needs Python 3.9+"
if ! python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else 1)' 2>/dev/null; then
    fail "python3 $(python3 -V 2>&1 | cut -d' ' -f2) is too old — ocstats needs 3.9+"
fi

fetch() { # fetch <url> <outfile>
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --max-time 60 "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -q --timeout=60 -O "$2" "$1"
    else
        fail "need curl or wget to download files"
    fi
}

mkdir -p "$BIN_DIR"
tmp=$(mktemp "$BIN_DIR/.ocstats.XXXXXX")
trap 'rm -f "$tmp"' EXIT

# --- main CLI ----------------------------------------------------------------

say "downloading ocstats ($REF) → $BIN_DIR/ocstats"
fetch "$REPO_RAW/$REF/ocstats" "$tmp"
if [[ -n "${OCSTATS_SHA256:-}" ]]; then
    echo "$OCSTATS_SHA256  $tmp" | sha256sum -c --quiet >/dev/null 2>&1 \
        || fail "checksum mismatch — expected OCSTATS_SHA256=$OCSTATS_SHA256"
fi
chmod +x "$tmp"
head -1 "$tmp" | grep -q '#!' || fail "downloaded file is not the CLI (bad ref? try --ref main)"
mv -f "$tmp" "$BIN_DIR/ocstats"

if [[ "$WITH_BASH" == "1" ]]; then
    command -v sqlite3 >/dev/null 2>&1 || fail "--with-bash needs sqlite3"
    command -v jq >/dev/null 2>&1 || fail "--with-bash needs jq"
    tmp2=$(mktemp "$BIN_DIR/.ocstats.sh.XXXXXX")
    fetch "$REPO_RAW/$REF/ocstats.sh" "$tmp2"
    chmod +x "$tmp2"
    mv -f "$tmp2" "$BIN_DIR/ocstats.sh"
fi

# --- verify ------------------------------------------------------------------

version=$("$BIN_DIR/ocstats" --version 2>&1) || fail "installed CLI failed to run"
say "installed: $version"

if [[ "$WITH_BASH" == "1" ]]; then
    say "installed: $BIN_DIR/ocstats.sh"
fi

case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) say ""
       say "note: $BIN_DIR is not on your PATH."
       say "      add this to your shell profile:"
       say "        export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

say ""
say "try it:  ocstats"
