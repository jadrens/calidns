# DNS Server

A Go authoritative DNS server with GeoIP-aware responses, a management API, and optional query logging.

It supports A, AAAA, TXT, CNAME, MX, NS, SRV, CAA, PTR, and SOA records, plus additional DNS record types through the `other` field. Zones can have country-specific record sets. Query and EDNS history can be stored in SQLite (the standalone default) or PostgreSQL.

## Build and run

Requirements: Go 1.26.4 (as specified in [go.mod](go.mod)) and a C compiler with CGO enabled for the SQLite driver.

```sh
mkdir -p local
go build -o local/dns-server ./src
./local/dns-server
```

On first start, the server generates `local/config.yaml` from the embedded [default template](src/config/default_config.yaml). Existing configuration files are not overwritten. Start it from the repository root so the default `local/` paths resolve as shown. Use `-config /path/to/config.yaml` to select another file; GeoIP data and the Geo cache are stored beside that file, and a relative `database.sqlite_path` is resolved from its directory.

The default DNS listener is UDP/TCP port 53, which may require elevated privileges. For local testing, set `server.listen: ":1053"` in `local/config.yaml`, add a zone, then restart:

```yaml
zones:
  example.com:
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

- `local/config.yaml`: server, API, cluster, database, and zone settings. The `local/` directory is ignored by Git and is intended for configuration, credentials, databases, GeoIP data, and local binaries.
- `database.type: sqlite`: stores query and EDNS history in `database.sqlite_path` (default `local/queries.db`). Any other type value uses PostgreSQL connection fields.
- `local/geoip.dat`: optional local GeoIP country database. The resolver checks the SQLite Geo cache before the local file and can use an API fallback. `server.enable_geoip_mmap` selects a file-backed index; `server.geoip_update_url` and `server.geoip_update_interval` enable automatic updates.
- `server.api.enabled`: enables the HTTP management API on `:3101` by default. Configure `server.api.tokens` before exposing it.

Documentation: [Configuration guide](docs/CONFIG_EN.md) · [API reference](docs/API_EN.md) · [配置文档](docs/CONFIG.md) · [API 文档](docs/API.md).

## Development

```sh
go test ./...
```

Some GeoIP tests start a local HTTP test server and therefore require loopback socket access. Source code is under `src/`; the Go module remains at the repository root.

## License

Copyright (C) 2026 jadrens <jadren@jadren.dev>.

This project is licensed under the GNU General Public License, version 3 only (`GPL-3.0-only`). See [LICENSE](LICENSE).
