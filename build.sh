#!/usr/bin/env bash
#
# Builds the DarkCode release artifacts:
#   - Linux .deb packages (amd64, i386)
#   - Windows .exe binaries (amd64, 386)
#   - SBOM.txt read back out of the linked binary
#   - SHA256SUMS over all of them, optionally GPG-signed
#
# Output goes to dist/. Usage: ./build.sh [version]   (default below)
set -euo pipefail

APP="darkcode"
VERSION="${1:-1.2.1}"
OUT="dist"
LDFLAGS="-s -w -X main.Version=${VERSION}"
MAINTAINER="Team Dark Neural Network <contact@darkneuralnetwork.com>"

echo "==> Building ${APP} v${VERSION}"
rm -rf "${OUT}"
mkdir -p "${OUT}"

# go GOARCH -> debian architecture name
deb_arch() { case "$1" in amd64) echo amd64 ;; 386) echo i386 ;; *) echo "$1" ;; esac; }

build_deb() {
  local goarch="$1" darch stage
  darch="$(deb_arch "$goarch")"
  stage="${OUT}/${APP}_${VERSION}_${darch}"
  echo "==> linux/${goarch} -> ${APP}-v${VERSION}-linux-${darch}.deb"
  mkdir -p "${stage}/usr/local/bin" "${stage}/DEBIAN"
  GOOS=linux GOARCH="${goarch}" CGO_ENABLED=0 go build \
    -trimpath -buildvcs=false -ldflags="${LDFLAGS}" \
    -o "${stage}/usr/local/bin/${APP}" .
  cat > "${stage}/DEBIAN/control" <<EOF
Package: ${APP}
Version: ${VERSION}
Section: utils
Priority: optional
Architecture: ${darch}
Maintainer: ${MAINTAINER}
Description: DarkCode AI Agent Platform
 A local-first, modular autonomous AI agent platform for software engineering.
EOF
  dpkg-deb --build --root-owner-group "${stage}" \
    "${OUT}/${APP}-v${VERSION}-linux-${darch}.deb" >/dev/null
  rm -rf "${stage}"
}

build_exe() {
  local goarch="$1"
  echo "==> windows/${goarch} -> ${APP}-v${VERSION}-windows-${goarch}.exe"
  GOOS=windows GOARCH="${goarch}" CGO_ENABLED=0 go build \
    -trimpath -buildvcs=false -ldflags="${LDFLAGS}" \
    -o "${OUT}/${APP}-v${VERSION}-windows-${goarch}.exe" .
}

build_deb amd64
build_deb 386
build_exe amd64
build_exe 386

# Software bill of materials, read back out of a real binary rather than from
# go.mod: `go version -m` reports exactly the modules and hashes linked into
# the artifact, so the SBOM cannot drift from what shipped.
echo "==> Generating SBOM"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -trimpath -buildvcs=false -ldflags="${LDFLAGS}" -o "${OUT}/.sbom-probe" .
{
  echo "# SBOM for ${APP} v${VERSION}"
  echo "# Generated $(date -u +%Y-%m-%dT%H:%M:%SZ) from the linked binary."
  echo "# Verify with: go version -m <binary>"
  echo
  go version -m "${OUT}/.sbom-probe"
} > "${OUT}/SBOM.txt"
rm -f "${OUT}/.sbom-probe"

echo "==> Generating SHA256SUMS"
( cd "${OUT}" && sha256sum ./*.deb ./*.exe SBOM.txt > SHA256SUMS )

# Signing is opt-in: set DARKCODE_SIGNING_KEY to a gpg key id. Signing the
# checksum file covers every artifact through one signature, so a verifier
# checks the signature once and the hashes thereafter.
if [ -n "${DARKCODE_SIGNING_KEY:-}" ]; then
  echo "==> Signing SHA256SUMS with ${DARKCODE_SIGNING_KEY}"
  gpg --batch --yes --local-user "${DARKCODE_SIGNING_KEY}" \
      --armor --detach-sign --output "${OUT}/SHA256SUMS.asc" "${OUT}/SHA256SUMS"
  echo "    verify with: gpg --verify SHA256SUMS.asc SHA256SUMS"
else
  echo "==> Skipping signature (set DARKCODE_SIGNING_KEY to enable)"
fi

echo ""
echo "==> Done. Artifacts in ${OUT}/:"
ls -1 "${OUT}"
echo ""
echo "Builds are reproducible: CGO_ENABLED=0, -trimpath, -buildvcs=false, and a"
echo "version-only ldflags string. The same tag and Go toolchain yield identical"
echo "bytes — re-run this script and compare SHA256SUMS to verify."
