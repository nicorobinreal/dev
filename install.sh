#!/bin/sh
# Install dev: https://github.com/nicorobinreal/dev
#
#   curl -fsSL https://raw.githubusercontent.com/nicorobinreal/dev/main/install.sh | sh
#
# Environment:
#   DEV_VERSION=v1.2.3   install that release instead of the latest
#   DEV_INSTALL_DIR=...  install directory (default ~/.local/bin)
#   DEV_SKIP_INIT=1      don't run `dev init` afterwards
#   DEV_INIT_ARGS="..."  flags for `dev init`, e.g. "--json --machine mbp"
#
# Agents: with no terminal, `dev init` doesn't prompt; it uses DEV_INIT_ARGS
# and defaults, e.g.
#   curl -fsSL .../install.sh | DEV_INIT_ARGS="--json" sh
set -eu

REPO="nicorobinreal/dev"
INSTALL_DIR="${DEV_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

[ "$(uname -s)" = "Darwin" ] || die "dev supports macOS only (found $(uname -s))"

case "$(uname -m)" in
  arm64 | aarch64) ARCH=arm64 ;;
  x86_64 | amd64) ARCH=amd64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

need curl
need tar
need shasum

VERSION="${DEV_VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) ||
    die "could not query the latest release"
  [ -n "$VERSION" ] || die "could not determine the latest release"
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

ASSET="dev_darwin_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/dev-install.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

say "Downloading dev $VERSION ($ARCH)..."
curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET" || die "download failed: $BASE/$ASSET"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" || die "download failed: $BASE/checksums.txt"

WANT=$(awk -v f="$ASSET" '$2 == f || $2 == "*" f { print $1; exit }' "$TMP/checksums.txt")
[ -n "$WANT" ] || die "$ASSET is not listed in checksums.txt"
GOT=$(shasum -a 256 "$TMP/$ASSET" | awk '{ print $1 }')
[ "$WANT" = "$GOT" ] || die "checksum mismatch for $ASSET (want $WANT, got $GOT)"

mkdir -p "$TMP/x"
tar -xzf "$TMP/$ASSET" -C "$TMP/x" dev || die "archive does not contain dev"
[ -f "$TMP/x/dev" ] || die "archive does not contain dev"

mkdir -p "$INSTALL_DIR"
# Copy next to the target first so the final mv is an atomic rename.
cp "$TMP/x/dev" "$INSTALL_DIR/.dev.install.$$"
chmod 755 "$INSTALL_DIR/.dev.install.$$"
mv -f "$INSTALL_DIR/.dev.install.$$" "$INSTALL_DIR/dev"
say "Installed $INSTALL_DIR/dev ($VERSION)"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    say ""
    say "warning: $INSTALL_DIR is not on your PATH. Add this to your shell profile:"
    say "  export PATH=\"$INSTALL_DIR:\$PATH\""
    say ""
    ;;
esac

if [ "${DEV_SKIP_INIT:-}" = "1" ]; then
  exit 0
fi

# In a terminal, `dev init` asks for each setting. Under `curl | sh`, stdin is
# this script, so read answers from the terminal when there is one. Without a
# terminal (an agent), `dev init` doesn't prompt and uses DEV_INIT_ARGS plus
# defaults. DEV_INIT_ARGS is split on spaces on purpose.
# shellcheck disable=SC2086
if [ -t 0 ]; then
  "$INSTALL_DIR/dev" init ${DEV_INIT_ARGS:-}
elif (exec </dev/tty) 2>/dev/null; then
  "$INSTALL_DIR/dev" init ${DEV_INIT_ARGS:-} </dev/tty
else
  "$INSTALL_DIR/dev" init ${DEV_INIT_ARGS:-} </dev/null
fi
