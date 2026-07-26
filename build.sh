#!/usr/bin/env bash
#
# Builds the DarkCode release artifacts:
#   - Linux .deb packages (amd64, i386)
#   - Windows .exe binaries (amd64, 386)
#   - SHA256SUMS over all of them
#
# Output goes to dist/. Usage: ./build.sh [version]   (default below)
set -euo pipefail

APP="darkcode"
VERSION="${1:-1.2.0}"
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

echo "==> Generating SHA256SUMS"
( cd "${OUT}" && sha256sum ./*.deb ./*.exe > SHA256SUMS )

echo ""
echo "==> Done. Artifacts in ${OUT}/:"
ls -1 "${OUT}"
