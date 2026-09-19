#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: $0 vMAJOR.MINOR.PATCH" >&2
  exit 2
}

version=${1:-}
[[ $# -eq 1 && $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || usage

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
release=${version#v}
arch=$(go env GOHOSTARCH)
[[ $(go env GOHOSTOS) == linux ]] || { echo 'Build on Linux.' >&2; exit 1; }
[[ $arch == amd64 || $arch == arm64 ]] || { echo "Unsupported architecture: $arch" >&2; exit 1; }

dist="$root/dist"
mkdir -p "$dist"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

require() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 1; }
  done
}

build_linux() {
  require go musl-gcc dpkg-deb rpmbuild tar zstd

  local binary="$work/dns-server"
  CGO_ENABLED=1 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$binary" ./src

  local stage="$work/deb"
  install -D -m 0755 "$binary" "$stage/usr/bin/dns-server"
  install -D -m 0644 README.md "$stage/usr/share/doc/dns-server/README.md"
  install -D -m 0644 LICENSE "$stage/usr/share/doc/dns-server/copyright"

  local deb_arch rpm_arch
  case "$arch" in
    amd64) deb_arch=amd64; rpm_arch=x86_64 ;;
    arm64) deb_arch=arm64; rpm_arch=aarch64 ;;
  esac
  mkdir -p "$stage/DEBIAN"
  cat > "$stage/DEBIAN/control" <<EOF
Package: dns-server
Version: $release
Section: net
Priority: optional
Architecture: $deb_arch
Maintainer: jadrens <jadren@jadren.dev>
Description: Authoritative DNS server with GeoIP-aware responses
EOF
  dpkg-deb --build --root-owner-group "$stage" "$dist/dns-server_${release}_${deb_arch}.deb"

  mkdir -p "$work/rpmbuild"/{BUILD,RPMS,SOURCES,SPECS,SRPMS}
  cat > "$work/rpmbuild/SPECS/dns-server.spec" <<EOF
Name: dns-server
Version: $release
Release: 1
Summary: Authoritative DNS server with GeoIP-aware responses
License: GPL-3.0-only
BuildArch: $rpm_arch

%description
Authoritative DNS server with GeoIP-aware responses and a management API.

%install
install -D -m 0755 $binary %{buildroot}/usr/bin/dns-server
install -D -m 0644 $root/README.md %{buildroot}/usr/share/doc/dns-server/README.md
install -D -m 0644 $root/LICENSE %{buildroot}/usr/share/doc/dns-server/LICENSE

%files
/usr/bin/dns-server
/usr/share/doc/dns-server/README.md
/usr/share/doc/dns-server/LICENSE
EOF
  mkdir -p "$work/rpmdb" "$work/rpmtmp"
  rpmbuild -bb \
    --define "_topdir $work/rpmbuild" \
    --define "_dbpath $work/rpmdb" \
    --define "_tmppath $work/rpmtmp" \
    "$work/rpmbuild/SPECS/dns-server.spec"
  cp "$work/rpmbuild/RPMS/$rpm_arch/dns-server-$release-1.$rpm_arch.rpm" "$dist/"

  install -D -m 0755 "$binary" "$work/archive/dns-server/dns-server"
  install -D -m 0644 README.md "$work/archive/dns-server/README.md"
  install -D -m 0644 LICENSE "$work/archive/dns-server/LICENSE"
  tar -cf - -C "$work/archive" dns-server | zstd -q -f -T0 -19 -o "$dist/dns-server_${release}_linux_${arch}.tar.zst"

  # SQLite uses CGO, so the portable binary needs a musl C compiler too.
  # Prefer the system GCC when a PATH shim named gcc does not support -specs.
  local real_gcc=${REALGCC:-gcc}
  if [[ -z ${REALGCC:-} && -x /usr/bin/gcc ]]; then real_gcc=/usr/bin/gcc; fi
  REALGCC="$real_gcc" CGO_ENABLED=1 GOOS=linux GOARCH="$arch" CC=musl-gcc CGO_LDFLAGS=-static \
    go build -buildvcs=false -trimpath -ldflags='-s -w' \
    -o "$dist/dns-server_${release}_linux_${arch}_musl.bin" ./src
}

build_dashboard() {
  require bun tar zstd
  [[ -f dashboard/package.json && -f dashboard/bun.lock ]] || {
    echo 'Dashboard submodule is missing. Run: git submodule update --init --recursive' >&2
    exit 1
  }

  (
    cd dashboard
    bun install --frozen-lockfile
    bun run build
  )

  local dashboard_stage="$work/dashboard-archive/calidns-dashboard"
  mkdir -p "$dashboard_stage"
  cp -a dashboard/dist/. "$dashboard_stage/"
  install -m 0644 dashboard/README.md "$dashboard_stage/README.md"
  install -m 0644 dashboard/LICENSE "$dashboard_stage/LICENSE"
  tar -cf - -C "$work/dashboard-archive" calidns-dashboard | \
    zstd -q -f -T0 -19 -o "$dist/calidns-dashboard_${release}.tar.zst"
}

build_linux

# The Vite output is architecture-independent. Build it once in the amd64
# release job to avoid uploading the same asset from both matrix jobs.
if [[ $arch == amd64 ]]; then
  build_dashboard
fi

echo 'Release assets:'
find "$dist" -maxdepth 1 -type f -name "dns-server_${release}_*" -print
find "$dist" -maxdepth 1 -type f -name "dns-server-${release}-*" -print
find "$dist" -maxdepth 1 -type f -name "calidns-dashboard_${release}.*" -print
