#!/bin/sh
# Installs collie-agent from its GitHub release:
#
#   curl -fsSL https://github.com/collies-ai/collie-agent/releases/latest/download/install.sh | sh
#
# Environment:
#   COLLIE_AGENT_VERSION      a release tag such as v0.1.0 (default: the latest release)
#   COLLIE_AGENT_INSTALL_DIR  where to put the binary (default: ~/.local/bin, or /usr/local/bin as root)
#
# It detects the OS (macOS or Linux) and the CPU (amd64 or arm64), downloads the archive and checksums.txt,
# verifies the SHA-256 checksum and installs the binary.
set -eu

REPO="collies-ai/collie-agent"
VERSION="${COLLIE_AGENT_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
fail() { printf 'collie-agent install: %s\n' "$*" >&2; exit 1; }

detect_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    *) fail "unsupported OS $(uname -s): collie-agent runs on macOS and Linux" ;;
  esac
}

detect_arch() {
  arch="$(uname -m)"
  case "$arch" in
    x86_64 | amd64)
      # A shell under Rosetta reports x86_64 on Apple silicon: install the native build.
      if [ "$(uname -s)" = Darwin ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ]; then
        echo arm64
      else
        echo amd64
      fi
      ;;
    arm64 | aarch64) echo arm64 ;;
    *) fail "unsupported CPU $arch: collie-agent is built for amd64 and arm64" ;;
  esac
}

download() { # url file
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1" || fail "could not download $1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1" || fail "could not download $1"
  else
    fail "curl or wget is required"
  fi
}

sha256() { # file
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "sha256sum or shasum is required to verify the download"
  fi
}

main() {
  os="$(detect_os)"
  arch="$(detect_arch)"
  archive="collie-agent_${os}_${arch}.tar.gz"
  if [ "$VERSION" = latest ]; then
    base="https://github.com/${REPO}/releases/latest/download"
  else
    base="https://github.com/${REPO}/releases/download/${VERSION}"
  fi

  if [ -n "${COLLIE_AGENT_INSTALL_DIR:-}" ]; then
    dir="$COLLIE_AGENT_INSTALL_DIR"
  elif [ "$(id -u)" = 0 ]; then
    dir="/usr/local/bin"
  else
    dir="${HOME}/.local/bin"
  fi

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  say "Downloading collie-agent ($VERSION, $os/$arch)..."
  download "$base/$archive" "$tmp/$archive"
  download "$base/checksums.txt" "$tmp/checksums.txt"

  expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")"
  [ -n "$expected" ] || fail "checksums.txt has no entry for $archive"
  actual="$(sha256 "$tmp/$archive")"
  [ "$expected" = "$actual" ] || fail "checksum mismatch for $archive (expected $expected, got $actual)"

  tar -xzf "$tmp/$archive" -C "$tmp" collie-agent || fail "could not extract $archive"
  mkdir -p "$dir" || fail "could not create $dir"
  # Write next to the target and rename, so a running collie-agent is replaced atomically.
  cp "$tmp/collie-agent" "$dir/.collie-agent.new" || fail "could not write to $dir"
  chmod 0755 "$dir/.collie-agent.new"
  mv -f "$dir/.collie-agent.new" "$dir/collie-agent"

  say "Installed $("$dir/collie-agent" version) to $dir/collie-agent"
  case ":${PATH}:" in
    *":${dir}:"*) ;;
    *) say "" && say "$dir is not in your PATH. Add it, e.g.: export PATH=\"$dir:\$PATH\"" ;;
  esac
  say ""
  say "Next:"
  say "  collie-agent pair              # approve this machine in collie"
  say "  collie-agent service install   # keep it running in the background"
}

main "$@"
