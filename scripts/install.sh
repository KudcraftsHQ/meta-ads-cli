#!/usr/bin/env bash
#
# Install meta-ads from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/KudcraftsHQ/meta-ads-cli/main/scripts/install.sh | bash
#
# Honours:
#   VERSION      a specific tag, e.g. v0.2.0 (default: latest)
#   INSTALL_DIR  where to put the binary (default: ~/.local/bin, or /usr/local/bin if writable)

set -euo pipefail

REPO="KudcraftsHQ/meta-ads-cli"
BINARY="meta-ads"

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
info() { printf 'install: %s\n' "$*" >&2; }

# Global, not a local in main(): the EXIT trap runs after main() has returned,
# so a function-scoped variable would be gone by then and `set -u` would turn a
# successful install into a non-zero exit.
TMP=""
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; return 0; }
trap cleanup EXIT

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }
need curl
need tar

detect_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin) os=macos ;;
    Linux)  os=linux ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  printf '%s-%s' "$os" "$arch"
}

latest_version() {
  local body
  body="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest")" || return 1

  # Deliberately no `grep -m1` or `head -1` here: either would close the pipe
  # early, curl would die writing to it, and `set -o pipefail` would take the
  # whole script down with it. `sed -n 1p` reads its input to the end.
  printf '%s\n' "$body" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
    | sed -n '1p'
}

choose_dir() {
  if [ -n "${INSTALL_DIR:-}" ]; then
    printf '%s' "$INSTALL_DIR"
  elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    printf '/usr/local/bin'
  else
    printf '%s/.local/bin' "$HOME"
  fi
}

main() {
  local platform version dir url
  platform="$(detect_platform)"

  version="${VERSION:-$(latest_version)}"
  [ -n "$version" ] || die "could not determine the latest release; set VERSION explicitly"

  # Archive names carry the version without its leading v.
  url="https://github.com/${REPO}/releases/download/${version}/meta-ads-cli-${version#v}-${platform}.tar.gz"

  dir="$(choose_dir)"
  mkdir -p "$dir"

  TMP="$(mktemp -d)"

  info "downloading ${version} for ${platform}"
  curl -fsSL "$url" -o "$TMP/archive.tar.gz" \
    || die "download failed: $url"

  tar -xzf "$TMP/archive.tar.gz" -C "$TMP"
  [ -f "$TMP/$BINARY" ] || die "archive did not contain $BINARY"

  install -m 0755 "$TMP/$BINARY" "$dir/$BINARY"
  info "installed $dir/$BINARY"

  case ":$PATH:" in
    *":$dir:"*) ;;
    *) info "note: $dir is not on your PATH" ;;
  esac

  "$dir/$BINARY" --version
}

main "$@"
