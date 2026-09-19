# CaliDNS

A Go authoritative DNS server with GeoIP-aware responses, a management API, and optional query logging.

It supports A, AAAA, TXT, CNAME, MX, NS, SRV, CAA, PTR, and SOA records, plus additional DNS record types through the `other` field. Zones can have country-specific record sets. Query and EDNS history can be stored in SQLite (the standalone default) or PostgreSQL.

## One-click install

Install the latest release on Linux amd64 or arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/jadrens/calidns/main/scripts/install.sh | sudo bash
```

The installer detects the distribution and architecture, verifies the release checksum, and chooses the native `.deb` or `.rpm` package where supported. Alpine, Arch, and other Linux distributions receive the static musl binary. It also creates `/etc/calidns` and installs a systemd or OpenRC service definition when either init system is active. It does not enable, start, restart, or stop the service; existing service state is unchanged. On a new installation, start it explicitly when ready, and its first start will generate `/etc/calidns/config.yaml`.

The generated configuration is `/etc/calidns/config.yaml`. To install a specific release, set `CALIDNS_VERSION`, for example:

```sh
curl -fsSL https://raw.githubusercontent.com/jadrens/calidns/main/scripts/install.sh | sudo CALIDNS_VERSION=v1.2.3 bash
```

Start the installed service when ready:

```sh
# systemd
sudo systemctl start calidns

# OpenRC
sudo rc-service calidns start
```

## Build and run

Requirements: Go 1.26.4 (as specified in [go.mod](go.mod)) and a C compiler with CGO enabled for the SQLite driver.

```sh
mkdir -p local
go build -o local/calidns ./src
./local/calidns
```

On first start, a directly run binary generates `config.yaml` in its current working directory. A binary installed in a system bin directory uses `/etc/calidns/config.yaml`. The file is generated from the embedded [default template](src/config/default_config.yaml), with port 53 listeners for the machine's active non-loopback IP addresses. Existing files are not overwritten. Use `-config /path/to/config.yaml` to select another file; GeoIP data and the Geo cache are stored beside that file, and a relative `database.sqlite_path` is resolved from its directory.

The generated DNS listeners use UDP/TCP port 53, which may require elevated privileges. For local testing, edit `config.yaml` to use a free address and port, such as `server.listen: ["127.0.0.1:1053"]`, add a zone, then restart:

```yaml
zones:
  example.com:
    mode: simple
    default:
      a: ["192.0.2.10"]
      aaaa: ["2001:db8::10"]
    ttl: 300
```

```sh
dig @127.0.0.1 -p 1053 example.com A
```

The generated configuration starts with no zones, so DNS requests will use `server.default_response` until a zone is configured. The HTTP API is disabled by default; before enabling it, replace the example Bearer token and restrict access appropriately.

## Configuration

- `config.yaml` (or `/etc/calidns/config.yaml` for system installs): server, API, cluster, database, and zone settings. The `local/` directory remains ignored by Git for local binaries and data.
- `database.type: sqlite`: stores query and EDNS history in `database.sqlite_path` (default `queries.db` beside the config file). Any other type value uses PostgreSQL connection fields.
- `geoip.dat` beside the config file: optional local GeoIP country database. The resolver checks the SQLite Geo cache before the local file and can use an API fallback. `server.enable_geoip_mmap` selects a file-backed index; `server.geoip_update_url` and `server.geoip_update_interval` enable automatic updates.
- `server.api.enabled`: enables the HTTP management API on `:3101` by default. Configure `server.api.tokens` before exposing it.

Documentation: [Configuration guide](docs/CONFIG_EN.md) · [API reference](docs/API_EN.md) · [配置文档](docs/CONFIG.md) · [API 文档](docs/API.md).

## Development

```sh
go test ./...
```

Some GeoIP tests start a local HTTP test server and therefore require loopback socket access. Source code is under `src/`; the Go module remains at the repository root.

## Releases

On Linux amd64 or arm64, run `bash scripts/build-release.sh v1.2.3` to create `calidns` `.deb`, `.rpm`, `.tar.zst`, and static musl `.bin` assets for the host architecture in `dist/`. On amd64 it also uses Bun and Vite to create the architecture-independent `calidns-dashboard_1.2.3.tar.zst` static dashboard package. Initialize the `dashboard` submodule first. The script requires Go, Bun on amd64, `dpkg-deb`, `rpmbuild`, `zstd`, and `musl-gcc`. Native packages install `/usr/bin/calidns` and a `calidns.service` definition, but do not enable or start it. The dashboard archive can be extracted into any static web root; configure that host to fall back to `index.html` for client-side routes.

To publish on GitHub, first commit and push the release changes to `main`, then create and push a tag pointing at that commit, for example `git tag v1.2.3 && git push origin main v1.2.3`. From the Actions tab, run **Publish release** from `main` and enter the tag. The workflow verifies that the remote tag exists, builds both Linux server architectures and the Vite dashboard from that tag, runs their tests, generates SHA-256 checksums, and publishes all assets to a GitHub release.

## License

Copyright (C) 2026 jadrens <jadren@jadren.dev>.

This project is licensed under the GNU General Public License, version 3 only (`GPL-3.0-only`). See [LICENSE](LICENSE).
