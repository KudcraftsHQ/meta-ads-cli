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
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/'
}

choose_dir() {
  if [ -n "${INSTALL_DIR:-}" ]; then
    printf '%s' "$INSTALL_DIR"
  elif [ -w /usr/local/bin ] 2>/dev/null; then
    printf '/usr/local/bin'
  else
    printf '%s/.local/bin' "$HOME"
  fi
}

main() {
  local platform version dir tmp url
  platform="$(detect_platform)"

  version="${VERSION:-$(latest_version)}"
  [ -n "$version" ] || die "could not determine the latest release; set VERSION explicitly"

  # Archive names carry the version without its leading v.
  url="https://github.com/${REPO}/releases/download/${version}/meta-ads-cli-${version#v}-${platform}.tar.gz"

  dir="$(choose_dir)"
  mkdir -p "$dir"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT

  info "downloading ${version} for ${platform}"
  curl -fsSL "$url" -o "$tmp/archive.tar.gz" \
    || die "download failed: $url"

  tar -xzf "$tmp/archive.tar.gz" -C "$tmp"
  [ -f "$tmp/$BINARY" ] || die "archive did not contain $BINARY"

  install -m 0755 "$tmp/$BINARY" "$dir/$BINARY"
  info "installed $dir/$BINARY"

  case ":$PATH:" in
    *":$dir:"*) ;;
    *) info "note: $dir is not on your PATH" ;;
  esac

  "$dir/$BINARY" --version
}

main "$@"
