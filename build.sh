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
VERSION="${1:-1.3.2}"

# SOURCE_DATE_EPOCH pins every timestamp that would otherwise be "now".
#
# The Go binaries are already reproducible — -trimpath and -buildvcs=false see
# to that — but the artifacts around them were not: dpkg-deb stamps each file
# with its mtime and the SBOM recorded the moment it was written. Two builds of
# the same commit therefore produced identical binaries inside .deb files with
# different checksums, which quietly made SHA256SUMS impossible to verify
# independently.
#
# Defaulting to the commit date rather than the clock means the same commit
# yields the same bytes on any day.
#
# Reproducibility is over source *and toolchain*, not source alone: the Go
# compiler is part of the input, so a different version produces different
# object code however clean the rest of the build is. CI pins itself to the
# version in go.mod for exactly this reason. To reproduce a published release,
# match the toolchain recorded in its SBOM.
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
  SOURCE_DATE_EPOCH="$(git log -1 --pretty=%ct 2>/dev/null || echo 0)"
fi
export SOURCE_DATE_EPOCH
BUILD_DATE="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
  || date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
  || echo unknown)"
OUT="dist"
# The stamp targets core.Version, the variable the whole binary reads. It used
# to name main.Version, which was never declared — and the linker accepts -X
# against a missing symbol without complaint, writes nothing, and exits zero.
# Every release therefore shipped reporting a hardcoded 1.0.0. The assertion
# after the build is what makes that failure loud instead of silent.
LDFLAGS="-s -w -X github.com/darkcode/core.Version=${VERSION}"
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
  # Every file gets the same timestamp, so the archive is a function of its
  # contents alone. Without this the .deb changes on every run even though the
  # binary inside it does not.
  find "${stage}" -exec touch -h -d "@${SOURCE_DATE_EPOCH}" {} +
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

# Assert the version stamp actually landed.
#
# `-X` against a symbol that does not exist is not an error: the linker writes
# nothing and exits zero. That is how every release up to 1.3.2 shipped
# reporting a hardcoded "1.0.0" while the build claimed to stamp it. Asking the
# binary what it thinks it is turns that silence into a failed build.
echo "==> Verifying the version stamp"
STAMP_PROBE="${OUT}/.version-probe"
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="${LDFLAGS}" -o "${STAMP_PROBE}" .
REPORTED="$("${STAMP_PROBE}" --version 2>/dev/null || true)"
rm -f "${STAMP_PROBE}"
if [ "${REPORTED}" != "${VERSION}" ]; then
  echo "    FAILED: binary reports '${REPORTED}', expected '${VERSION}'" >&2
  echo "    The -X stamp in LDFLAGS is not reaching the variable it names." >&2
  exit 1
fi
echo "    binary reports ${REPORTED}"

# Software bill of materials, read back out of a real binary rather than from
# go.mod: `go version -m` reports exactly the modules and hashes linked into
# the artifact, so the SBOM cannot drift from what shipped.
echo "==> Generating SBOM"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
  -trimpath -buildvcs=false -ldflags="${LDFLAGS}" -o "${OUT}/.sbom-probe" .
{
  echo "# SBOM for ${APP} v${VERSION}"
  echo "# Generated ${BUILD_DATE} from the linked binary (source date, not build time)."
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
