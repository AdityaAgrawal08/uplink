#!/bin/sh
set -e

# Repository configuration
REPO="AdityaAgrawal08/uplink-delta"
BINARY_NAME="uplink"

# Detect OS
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "${OS}" in
  linux*)   OS="linux" ;;
  darwin*)  OS="darwin" ;;
  *)        echo "Error: Unsupported OS: ${OS}" >&2; exit 1 ;;
esac

# Detect Architecture
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)       echo "Error: Unsupported architecture: ${ARCH}" >&2; exit 1 ;;
esac

# Define download URL for pre-built asset using the GitHub latest release redirect
# e.g., uplink-linux-amd64.tar.gz
ASSET_NAME="${BINARY_NAME}-${OS}-${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${ASSET_NAME}"

# Temporary directory for download
TEMP_DIR=$(mktemp -d)
CLEANUP() {
  rm -rf "${TEMP_DIR}"
}
trap CLEANUP EXIT

echo "Downloading ${DOWNLOAD_URL}..."
curl -sSL -o "${TEMP_DIR}/${ASSET_NAME}" "${DOWNLOAD_URL}"

# Verify checksum against the published checksums.txt (fail closed: a
# missing or mismatched checksum aborts before anything executes).
echo "Verifying checksum..."
CHECKSUM_URL="https://github.com/${REPO}/releases/latest/download/checksums.txt"
EXPECTED=$(curl -sSL "${CHECKSUM_URL}" | awk -v asset="${ASSET_NAME}" '$2 == asset {print $1}')
if [ -z "${EXPECTED}" ]; then
  echo "Error: no checksum entry for ${ASSET_NAME} — aborting." >&2
  exit 1
fi
ACTUAL=""
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TEMP_DIR}/${ASSET_NAME}" | cut -d' ' -f1)"
elif command -v shasum >/dev/null 2>&1; then
  # macOS ships shasum, not sha256sum.
  ACTUAL="$(shasum -a 256 "${TEMP_DIR}/${ASSET_NAME}" | cut -d' ' -f1)"
else
  echo "Error: need sha256sum or shasum to verify the download — aborting." >&2
  exit 1
fi
if [ "${ACTUAL}" != "${EXPECTED}" ]; then
  echo "Error: checksum mismatch — aborting (do not run this binary)." >&2
  exit 1
fi
echo "Checksum OK."

# Extract and install
echo "Extracting binary..."
tar -xzf "${TEMP_DIR}/${ASSET_NAME}" -C "${TEMP_DIR}"

INSTALL_DIR="/usr/local/bin"
if [ -n "${PREFIX}" ]; then
  INSTALL_DIR="${PREFIX}/bin"
fi

echo "Installing to ${INSTALL_DIR}/${BINARY_NAME}..."

mkdir -p "${INSTALL_DIR}"
if [ -w "${INSTALL_DIR}" ]; then
  mv "${TEMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
else
  if command -v sudo >/dev/null 2>&1; then
    echo "Write permission denied for ${INSTALL_DIR}. Prompting for sudo..."
    sudo mv "${TEMP_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
  else
    LOCAL_BIN="${HOME}/.local/bin"
    echo "Write permission denied for ${INSTALL_DIR} and sudo is not available."
    echo "Attempting installation to ${LOCAL_BIN}..."
    mkdir -p "${LOCAL_BIN}"
    mv "${TEMP_DIR}/${BINARY_NAME}" "${LOCAL_BIN}/${BINARY_NAME}"
    INSTALL_DIR="${LOCAL_BIN}"
  fi
fi

chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
echo "Successfully installed ${BINARY_NAME} to ${INSTALL_DIR}/${BINARY_NAME}."

if [ "${INSTALL_DIR}" = "${HOME}/.local/bin" ]; then
  echo "⚠️  Important: Make sure '${INSTALL_DIR}' is in your PATH environment variable."
  echo "You can add it by running: echo 'export PATH=\"\$PATH:${INSTALL_DIR}\"' >> ~/.bashrc && source ~/.bashrc"
else
  echo "You can now run '${BINARY_NAME}' from anywhere in your terminal."
fi
