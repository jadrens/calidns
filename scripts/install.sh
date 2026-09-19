#!/usr/bin/env bash
set -euo pipefail

repo=${CALIDNS_REPOSITORY:-jadrens/calidns}
requested_version=${CALIDNS_VERSION:-latest}
config_dir=/etc/calidns
curl_args=(--fail --location --retry 1 --retry-delay 1 --connect-timeout 5 --max-time 15)

log() {
  printf '[calidns] %s\n' "$*"
}

fail() {
  printf '[calidns] error: %s\n' "$*" >&2
  exit 1
}

require() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

[[ $(uname -s) == Linux ]] || fail 'only Linux is supported'
[[ ${EUID:-$(id -u)} -eq 0 ]] || fail 'run this installer as root (for example, pipe it to sudo bash)'
require curl
require install

case $(uname -m) in
  x86_64 | amd64)
    release_arch=amd64
    rpm_arch=x86_64
    ;;
  aarch64 | arm64)
    release_arch=arm64
    rpm_arch=aarch64
    ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [[ -r /etc/os-release ]]; then
  # ID and ID_LIKE are data supplied by the local operating system.
  # shellcheck disable=SC1091
  source /etc/os-release
else
  ID=unknown
  ID_LIKE=
fi
distro=${ID:-unknown}
distro_family=" ${ID:-} ${ID_LIKE:-} "

case $distro_family in
  *' debian '* | *' ubuntu '*) install_kind=deb ;;
  *' rhel '* | *' fedora '* | *' centos '* | *' rocky '* | *' almalinux '* | *' ol '* | *' suse '* | *' opensuse '*) install_kind=rpm ;;
  *) install_kind=binary ;;
esac

tag=
if [[ $requested_version == latest ]]; then
  log 'detecting the latest release'
  release_page=$(curl "${curl_args[@]}" -sS -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") || \
    fail "could not find the latest release for $repo"
  tag=${release_page%%\?*}
  tag=${tag%/}
  tag=${tag##*/}
else
  tag=$requested_version
  [[ $tag == v* ]] || tag="v$tag"
fi
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "invalid release version: $tag"
version=${tag#v}
base_url="https://github.com/$repo/releases/download/$tag"

case $install_kind in
  deb) asset="calidns_${version}_${release_arch}.deb" ;;
  rpm) asset="calidns-${version}-1.${rpm_arch}.rpm" ;;
  binary) asset="calidns_${version}_linux_${release_arch}_musl.bin" ;;
esac

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
chmod 0755 "$work_dir"
asset_path="$work_dir/$asset"
checksums_path="$work_dir/SHA256SUMS"

log "detected $distro on $release_arch; downloading $asset"
curl "${curl_args[@]}" -sS -o "$checksums_path" "$base_url/SHA256SUMS"
curl "${curl_args[@]}" -sS -o "$asset_path" "$base_url/$asset"

expected_checksum=$(awk -v name="$asset" '$2 == name || $2 == ("*" name) { print $1; exit }' "$checksums_path")
[[ -n $expected_checksum ]] || fail "$asset is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum=$(sha256sum "$asset_path" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual_checksum=$(shasum -a 256 "$asset_path" | awk '{print $1}')
else
  fail 'sha256sum or shasum is required to verify the download'
fi
[[ $actual_checksum == "$expected_checksum" ]] || fail "checksum verification failed for $asset"
log 'checksum verified'

case $install_kind in
  deb)
    if command -v apt-get >/dev/null 2>&1; then
      apt-get install -y "$asset_path"
    elif command -v dpkg >/dev/null 2>&1; then
      dpkg -i "$asset_path"
    else
      fail 'no Debian package installer was found'
    fi
    binary_path=/usr/bin/calidns
    ;;
  rpm)
    if command -v dnf >/dev/null 2>&1; then
      dnf install -y "$asset_path"
    elif command -v yum >/dev/null 2>&1; then
      yum install -y "$asset_path"
    elif command -v zypper >/dev/null 2>&1; then
      zypper --non-interactive --no-gpg-checks install "$asset_path"
    elif command -v rpm >/dev/null 2>&1; then
      rpm -Uvh "$asset_path"
    else
      fail 'no RPM package installer was found'
    fi
    binary_path=/usr/bin/calidns
    ;;
  binary)
    install -D -m 0755 "$asset_path" /usr/local/bin/calidns
    binary_path=/usr/local/bin/calidns
    ;;
esac

[[ -x $binary_path ]] || fail "calidns was not installed at $binary_path"

service_user=root
service_group=root
if id -u calidns >/dev/null 2>&1; then
  service_user=calidns
elif command -v useradd >/dev/null 2>&1; then
  nologin_shell=$(command -v nologin || printf '/usr/sbin/nologin')
  useradd --system --home-dir "$config_dir" --shell "$nologin_shell" calidns
  service_user=calidns
elif command -v adduser >/dev/null 2>&1; then
  nologin_shell=$(command -v nologin || printf '/sbin/nologin')
  adduser -S -D -H -h "$config_dir" -s "$nologin_shell" calidns
  service_user=calidns
fi
if [[ $service_user == calidns ]]; then
  service_group=$(id -gn calidns)
fi

install -d -m 0750 "$config_dir"
if [[ $service_user == calidns ]]; then
  chown -R "$service_user:$service_group" "$config_dir"
fi

installed_service=
if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  log 'installing systemd service'
  cat > /etc/systemd/system/calidns.service <<EOF
[Unit]
Description=CaliDNS authoritative DNS server
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=$service_user
Group=$service_group
WorkingDirectory=$config_dir
ExecStart=$binary_path -config $config_dir/config.yaml
Restart=on-failure
RestartSec=5s
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=$config_dir

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  installed_service=systemd
elif command -v rc-service >/dev/null 2>&1 && command -v rc-update >/dev/null 2>&1; then
  openrc_user=root
  openrc_group=root
  if [[ $service_user == calidns ]] && command -v setcap >/dev/null 2>&1 && setcap cap_net_bind_service=+ep "$binary_path"; then
    openrc_user=calidns
    openrc_group=$service_group
  fi
  log 'installing OpenRC service'
  cat > /etc/init.d/calidns <<EOF
#!/sbin/openrc-run
name="CaliDNS"
description="CaliDNS authoritative DNS server"
command="$binary_path"
command_args="-config $config_dir/config.yaml"
command_user="$openrc_user:$openrc_group"
directory="$config_dir"
command_background=true
pidfile="/run/calidns.pid"

depend() {
  need net
}
EOF
  chmod 0755 /etc/init.d/calidns
  installed_service=openrc
fi

log "installed CaliDNS $tag at $binary_path"
log "configuration: $config_dir/config.yaml"
case $installed_service in
  systemd)
    log 'the systemd service was installed; its enabled and running state was not changed'
    log 'start it manually: systemctl start calidns'
    log 'enable it at boot: systemctl enable calidns'
    ;;
  openrc)
    log 'the OpenRC service was installed; its enabled and running state was not changed'
    log 'start it manually: rc-service calidns start'
    log 'enable it at boot: rc-update add calidns default'
    ;;
  *)
    log "no supported init system is active; start it with: $binary_path -config $config_dir/config.yaml"
    ;;
esac
