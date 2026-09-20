# CaliDNS

A Go authoritative DNS server with GeoIP-aware responses, a management API, and optional query logging.

It supports A, AAAA, TXT, CNAME, MX, NS, SRV, CAA, PTR, and SOA records, plus additional DNS record types through the `other` field. Zones can have country-specific record sets. Query and EDNS history can be stored in SQLite (the standalone default) or PostgreSQL.

## One-click install

Install the latest release on Linux amd64 or arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/jadrens/calidns/main/scripts/install.sh | sudo bash
```

The installer detects the distribution and architecture, verifies the release checksum, and chooses the native `.deb` or `.rpm` package where supported. It creates `/etc/calidns` for configuration and `/var/lib/calidns` for mutable data, then installs a systemd or OpenRC service definition. It does not enable or start the service. On first start the service generates `/etc/calidns/config.yaml` and initializes its databases under `/var/lib/calidns`.

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

On first start, a directly run binary generates `config.yaml` and its data files in the current working directory. A packaged service keeps static configuration in `/etc/calidns/` and mutable data in `/var/lib/calidns/`. `config.yaml` is generated from the embedded [default template](src/config/default_config.yaml) and is never rewritten by the Web API. Use `-config` and `-data-dir` to override these locations.

The generated DNS listeners use UDP/TCP port 53, which may require elevated privileges. For local testing, edit `config.yaml` to use a free address and port, such as `server.listen: ["127.0.0.1:1053"]`, enable the HTTP API, and add a zone through the API or dashboard.

```sh
dig @127.0.0.1 -p 1053 example.com A
```

The DNS database starts with no zones and defaults to TTL 300, query recording disabled, and REFUSED for unmatched requests. The HTTP API is disabled by default; before enabling it, replace the example Bearer token and restrict access appropriately.

## Configuration

- `/etc/calidns/config.yaml` for package installs: static server, API, cluster, database, GeoIP update, and external integration settings.
- `/var/lib/calidns/dns_data.db`: DNS zones plus `default_ttl`, `default_record`, and `default_response`.
- `/var/lib/calidns/queries.db`: query and EDNS history when `database.type` is `sqlite` and the default relative path is used.
- `/var/lib/calidns/geo_cache.db` and `geoip.dat`: Geo cache and optional local GeoIP database.
- `server.api.enabled`: enables the HTTP management API on `:3101` by default. Configure `server.api.tokens` before exposing it.

Documentation: [Configuration guide](docs/CONFIG_EN.md) · [API reference](docs/API_EN.md) · [配置文档](docs/CONFIG.md) · [API 文档](docs/API.md).

## Development

```sh
(cd dashboard && bun install --frozen-lockfile && VITE_BASE_PATH=/dashboard/ bun run build -- --outDir ../internal/dashboard/dist --emptyOutDir)
go test ./...
```

The dashboard build is required because the Go binary embeds `internal/dashboard/dist`. Some GeoIP tests start a local HTTP test server and therefore require loopback socket access. Source code is under `src/`; the Go module remains at the repository root.

## Releases

On Linux amd64 or arm64, run `bash scripts/build-release.sh v1.2.3` to create `calidns` `.deb`, `.rpm`, `.tar.zst`, and static musl `.bin` assets for the host architecture in `dist/`. The script first builds the dashboard, which is embedded in every server binary and served at `/dashboard` when `server.api.dashboard.url` is `default`. A remote HTTP(S) ZIP containing the same `dist` tree can be configured instead and refreshed on `server.api.dashboard.update_interval`. The amd64 job also creates architecture-independent `.tar.zst` and `.zip` dashboard packages; the ZIP has `index.html` and `assets/` at its root and can be used directly as `dashboard.url`. Initialize the `dashboard` submodule first. The script requires Go, Bun, `dpkg-deb`, `rpmbuild`, `zstd`, `zip`, and `musl-gcc`. Native packages install `/usr/bin/calidns` and a `calidns.service` definition, but do not enable or start it. The standalone dashboard archive can be extracted into any static web root; configure that host to fall back to `index.html` for client-side routes.

To publish on GitHub, first commit and push the release changes to `main`, then create and push a tag pointing at that commit, for example `git tag v1.2.3 && git push origin main v1.2.3`. From the Actions tab, run **Publish release** from `main` and enter the tag. The workflow verifies that the remote tag exists, builds both Linux server architectures and the Vite dashboard from that tag, runs their tests, generates SHA-256 checksums, and publishes all assets to a GitHub release.

## License

Copyright (C) 2026 jadrens <jadren@jadren.dev>.

This project is licensed under the GNU General Public License, version 3 only (`GPL-3.0-only`). See [LICENSE](LICENSE).
