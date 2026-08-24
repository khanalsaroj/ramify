#!/usr/bin/env bash
# Ramify installer for Linux and macOS — installs both `ramify` and `ramifyd`.
#
#   curl -fsSL https://raw.githubusercontent.com/khanalsaroj/ramify/main/scripts/install.sh | bash
#
# Environment overrides:
#   RAMIFY_VERSION       install a specific version (e.g. v0.3.1), default: latest
#   RAMIFY_INSTALL_DIR   install location, default: /usr/local/bin (falls back to ~/.local/bin)
set -euo pipefail

REPO="khanalsaroj/ramify"
BINS="ramify ramifyd"
VERSION="${RAMIFY_VERSION:-latest}"
INSTALL_DIR="${RAMIFY_INSTALL_DIR:-}"

# ---------- Output helpers ----------
info() { printf '  %s\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*" >&2; }
die()  { printf '  \033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

banner() {
  printf '\n  \033[32mramify\033[0m — every branch becomes a live URL\n\n'
}

# ---------- Platform detection ----------
detect_platform() {
  OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  ARCH="$(uname -m)"

  case "$OS" in
    linux | darwin) ;;
    *) die "unsupported operating system: $OS (ramify ships prebuilt binaries for linux and darwin only — see 'From source' in the README)" ;;
  esac

  case "$ARCH" in
    x86_64 | amd64) ARCH="amd64" ;;
    arm64 | aarch64) ARCH="arm64" ;;
    *) die "unsupported architecture: $ARCH (ramify ships amd64/arm64 only — see 'From source' in the README)" ;;
  esac
}

# ---------- Version resolution ----------
resolve_version() {
  if [ "$VERSION" != "latest" ]; then
    printf '%s' "${VERSION#v}"
    return
  fi
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
    grep -o '"tag_name"[ ]*:[ ]*"[^"]*"' | head -1 | cut -d'"' -f4 | sed 's/^v//'
}

# ---------- Checksum verification (best effort) ----------
verify_checksum() {
  local dir="$1" asset="$2" version="$3" tool expected actual
  if command -v sha256sum >/dev/null 2>&1; then
    tool="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    tool="shasum -a 256"
  else
    warn "no sha256 tool found — skipping checksum verification"
    return
  fi

  if ! curl -fsSL "https://github.com/${REPO}/releases/download/v${version}/checksums.txt" -o "$dir/checksums.txt"; then
    warn "checksums.txt unavailable — skipping checksum verification"
    return
  fi

  expected="$(awk -v f="$asset" '$2 == f {print $1}' "$dir/checksums.txt" | head -1)"
  if [ -z "$expected" ]; then
    warn "no checksum entry for ${asset} — skipping verification"
    return
  fi
  actual="$($tool "$dir/$asset" | awk '{print $1}')"
  [ "$expected" = "$actual" ] || die "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"
  ok "checksum verified"
}

# ---------- Install location ----------
choose_install_dir() {
  if [ -n "$INSTALL_DIR" ]; then
    printf '%s' "$INSTALL_DIR"
    return
  fi
  for d in /usr/local/bin /opt/homebrew/bin; do
    if [ -d "$d" ]; then
      printf '%s' "$d"
      return
    fi
  done
  printf '%s' "$HOME/.local/bin"
}

install_binary() {
  local src="$1" name="$2" dir="$3"
  mkdir -p "$dir" 2>/dev/null || true
  if [ -w "$dir" ]; then
    install -m 0755 "$src" "$dir/$name"
  elif command -v sudo >/dev/null 2>&1; then
    warn "elevated permissions required to write to ${dir}"
    sudo install -m 0755 "$src" "$dir/$name"
  else
    die "cannot write to ${dir} and sudo is unavailable — set RAMIFY_INSTALL_DIR to a writable directory"
  fi
}

# ---------- Main ----------
main() {
  banner
  need curl
  need tar
  need uname

  detect_platform

  local version
  version="$(resolve_version)"
  [ -n "$version" ] || die "could not resolve the latest version — has a release been published yet? see https://github.com/${REPO}/releases"
  info "Installing ramify + ramifyd v${version} for ${OS}/${ARCH}"

  local tmp asset url
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  asset="ramify-${OS}-${ARCH}.tar.gz"
  url="https://github.com/${REPO}/releases/download/v${version}/${asset}"

  info "Downloading ${url}"
  curl -fSL "$url" -o "$tmp/$asset" || die "download failed — does a release exist for ${OS}/${ARCH}?"

  verify_checksum "$tmp" "$asset" "$version"

  tar -xzf "$tmp/$asset" -C "$tmp" || die "failed to extract archive"

  local dir
  dir="$(choose_install_dir)"

  for bin in $BINS; do
    local src="$tmp/$bin"
    if [ ! -f "$src" ]; then
      src="$(find "$tmp" -type f -name "$bin" 2>/dev/null | head -1)"
    fi
    [ -n "$src" ] && [ -f "$src" ] || die "could not find '$bin' inside the archive"
    chmod +x "$src"
    install_binary "$src" "$bin" "$dir"
  done

  hash -r 2>/dev/null || true
  if command -v ramify >/dev/null 2>&1; then
    ok "installed: $(command -v ramify)"
    ok "installed: $(command -v ramifyd 2>/dev/null || printf '%s/ramifyd' "$dir")"
    ramify --version || true
  else
    ok "installed to ${dir}/{ramify,ramifyd}"
    warn "${dir} is not on your PATH yet. Add it with:"
    warn "    export PATH=\"${dir}:\$PATH\""
  fi
  printf '\n'
  ok "Done! Next: ramify install --config-dir /etc/ramify --data-dir /var/lib/ramify"
  ok "See: https://github.com/${REPO}/blob/main/docs/quickstart.md"
}

main "$@"
