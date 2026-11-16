#!/usr/bin/env bash
# Deploy or update ~/bin/agent-bot for one or more Linux service users (e.g. grok, codex)
# with SHA-256 verification and atomic rename.
set -euo pipefail

REPO="${UPGRADE_REPO:-rshun/agent-bot}"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ASSET="agent-bot-linux-amd64" ;;
  aarch64|arm64) ASSET="agent-bot-linux-arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

USERS=("$@")
if [ "${#USERS[@]}" -eq 0 ]; then
  USERS=("grok" "codex")
fi

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

echo "==> Fetching latest release metadata from https://github.com/${REPO} ..."
TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "$TAG" ]; then
  echo "Failed to resolve latest release tag for ${REPO}" >&2
  exit 1
fi

BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
echo "==> Downloading ${ASSET} (${TAG}) and SHA256SUMS ..."
curl -fsSL -o "${TMP_DIR}/${ASSET}" "${BASE_URL}/${ASSET}"
curl -fsSL -o "${TMP_DIR}/SHA256SUMS" "${BASE_URL}/SHA256SUMS"

(
  cd "$TMP_DIR"
  grep "  ${ASSET}\$" SHA256SUMS | sha256sum --check -
)
chmod 0755 "${TMP_DIR}/${ASSET}"
"${TMP_DIR}/${ASSET}" --version

for u in "${USERS[@]}"; do
  if ! id "$u" >/dev/null 2>&1; then
    echo "Skipping non-existent user: $u"
    continue
  fi
  HOME_DIR="$(getent passwd "$u" | cut -d: -f6)"
  BIN_DIR="${HOME_DIR}/bin"
  install -d -m 0755 -o "$u" -g "$u" "$BIN_DIR"
  STAGED="${BIN_DIR}/.agent-bot.install.tmp"
  TARGET="${BIN_DIR}/agent-bot"
  install -m 0755 -o "$u" -g "$u" "${TMP_DIR}/${ASSET}" "$STAGED"
  if [ -f "$TARGET" ]; then
    cp -p "$TARGET" "${TARGET}.prev"
    chown "$u:$u" "${TARGET}.prev"
  fi
  mv -f "$STAGED" "$TARGET"
  echo "==> Installed ${TAG} -> ${TARGET} (owner ${u}:${u})"
done
