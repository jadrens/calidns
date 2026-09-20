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

  local binary="$work/calidns"
  CGO_ENABLED=1 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags='-s -w' -o "$binary" ./src

  local stage="$work/deb"
  install -D -m 0755 "$binary" "$stage/usr/bin/calidns"
  install -D -m 0644 scripts/calidns.service "$stage/lib/systemd/system/calidns.service"
  install -D -m 0644 README.md "$stage/usr/share/doc/calidns/README.md"
  install -D -m 0644 LICENSE "$stage/usr/share/doc/calidns/copyright"

  local deb_arch rpm_arch
  case "$arch" in
    amd64) deb_arch=amd64; rpm_arch=x86_64 ;;
    arm64) deb_arch=arm64; rpm_arch=aarch64 ;;
  esac
  mkdir -p "$stage/DEBIAN"
  cat > "$stage/DEBIAN/control" <<EOF
Package: calidns
Version: $release
Section: net
Priority: optional
Architecture: $deb_arch
Maintainer: jadrens <jadren@jadren.dev>
Conflicts: dns-server
Replaces: dns-server
Provides: dns-server
Description: Authoritative DNS server with GeoIP-aware responses
EOF
  cat > "$stage/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
exit 0
EOF
  cat > "$stage/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
exit 0
EOF
  chmod 0755 "$stage/DEBIAN/postinst" "$stage/DEBIAN/postrm"
  dpkg-deb --build --root-owner-group "$stage" "$dist/calidns_${release}_${deb_arch}.deb"

  mkdir -p "$work/rpmbuild"/{BUILD,RPMS,SOURCES,SPECS,SRPMS}
  cat > "$work/rpmbuild/SPECS/calidns.spec" <<EOF
Name: calidns
Version: $release
Release: 1
Summary: Authoritative DNS server with GeoIP-aware responses
License: GPL-3.0-only
BuildArch: $rpm_arch
Obsoletes: dns-server < $release-1
Provides: dns-server = $release-1

%description
Authoritative DNS server with GeoIP-aware responses and a management API.

%install
install -D -m 0755 $binary %{buildroot}/usr/bin/calidns
install -D -m 0644 $root/scripts/calidns.service %{buildroot}/usr/lib/systemd/system/calidns.service
install -D -m 0644 $root/README.md %{buildroot}/usr/share/doc/calidns/README.md
install -D -m 0644 $root/LICENSE %{buildroot}/usr/share/doc/calidns/LICENSE

%post
systemctl daemon-reload >/dev/null 2>&1 || :

%postun
systemctl daemon-reload >/dev/null 2>&1 || :

%files
/usr/bin/calidns
/usr/lib/systemd/system/calidns.service
/usr/share/doc/calidns/README.md
/usr/share/doc/calidns/LICENSE
EOF
  mkdir -p "$work/rpmdb" "$work/rpmtmp"
  rpmbuild -bb \
    --define "_topdir $work/rpmbuild" \
    --define "_dbpath $work/rpmdb" \
    --define "_tmppath $work/rpmtmp" \
    "$work/rpmbuild/SPECS/calidns.spec"
  cp "$work/rpmbuild/RPMS/$rpm_arch/calidns-$release-1.$rpm_arch.rpm" "$dist/"

  install -D -m 0755 "$binary" "$work/archive/calidns/calidns"
  install -D -m 0644 scripts/calidns.service "$work/archive/calidns/calidns.service"
  install -D -m 0644 README.md "$work/archive/calidns/README.md"
  install -D -m 0644 LICENSE "$work/archive/calidns/LICENSE"
  tar -cf - -C "$work/archive" calidns | zstd -q -f -T0 -19 -o "$dist/calidns_${release}_linux_${arch}.tar.zst"

  # SQLite uses CGO, so the portable binary needs a musl C compiler too.
  # Prefer the system GCC when a PATH shim named gcc does not support -specs.
  local real_gcc=${REALGCC:-gcc}
  if [[ -z ${REALGCC:-} && -x /usr/bin/gcc ]]; then real_gcc=/usr/bin/gcc; fi
  REALGCC="$real_gcc" CGO_ENABLED=1 GOOS=linux GOARCH="$arch" CC=musl-gcc CGO_LDFLAGS=-static \
    go build -buildvcs=false -trimpath -ldflags='-s -w' \
    -o "$dist/calidns_${release}_linux_${arch}_musl.bin" ./src
}

build_dashboard() {
  require bun tar zstd zip
  [[ -f dashboard/package.json && -f dashboard/bun.lock ]] || {
    echo 'Dashboard submodule is missing. Run: git submodule update --init --recursive' >&2
    exit 1
  }

  (
    cd dashboard
    bun install --frozen-lockfile
    VITE_BASE_PATH=/dashboard/ bun run build -- --outDir ../internal/dashboard/dist --emptyOutDir
  )
  [[ -f internal/dashboard/dist/index.html ]] || {
    echo 'Dashboard build did not produce internal/dashboard/dist/index.html' >&2
    exit 1
  }

  # Every server architecture embeds dashboard/dist. Only publish the separate,
  # architecture-independent dashboard archive from the amd64 job.
  [[ $arch == amd64 ]] || return 0

  local dashboard_stage="$work/dashboard-archive/calidns-dashboard"
  mkdir -p "$dashboard_stage"
  cp -a internal/dashboard/dist/. "$dashboard_stage/"
  install -m 0644 dashboard/README.md "$dashboard_stage/README.md"
  install -m 0644 dashboard/LICENSE "$dashboard_stage/LICENSE"
  tar -cf - -C "$work/dashboard-archive" calidns-dashboard | \
    zstd -q -f -T0 -19 -o "$dist/calidns-dashboard_${release}.tar.zst"
  (
    cd internal/dashboard/dist
    zip -q -r "$dist/calidns-dashboard_${release}.zip" .
  )
}

# Build before the Go binary so dashboard/dist is embedded. The separate static
# archive is emitted only by the amd64 job.
build_dashboard

build_linux

echo 'Release assets:'
find "$dist" -maxdepth 1 -type f -name "calidns_${release}_*" -print
find "$dist" -maxdepth 1 -type f -name "calidns-${release}-*" -print
find "$dist" -maxdepth 1 -type f -name "calidns-dashboard_${release}.*" -print
