# DNS Server Configuration Guide

[README](../README.md) · [API reference](API_EN.md) · [中文](CONFIG.md)

Package installs keep static configuration at `/etc/calidns/config.yaml` and mutable state under `/var/lib/calidns/`. A directly run development binary uses the current directory for both. Use `-config` and `-data-dir` to override the locations.

The static `config.yaml` is never rewritten by the Web API. Zones and the three API-managed defaults are stored in the data directory's `dns_data.db`. Legacy zones and defaults in YAML are not imported.

## Complete example

Replace the example database password and API token before deployment.

```yaml
server:
  listen:
    - "192.0.2.42:53"
    - "[2001:db8::42]:53"
  enable_geoip_mmap: false        # false: Go heap index; true: file-backed mmap index
  geoip_update_url: ""            # Direct geoip.dat URL; empty disables auto-updates
  geoip_update_interval: "24h"    # Next update is based on geoip.dat's mtime
  api:
    enabled: true
    listen: ":3101"
    tokens:
      - "replace-with-a-long-random-token"
    cors:
      allow_origins:
        - "https://admin.example.com"
      allow_methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
      allow_headers: ["Content-Type", "Authorization"]
      expose_headers: []
      max_age: 600
      allow_credentials: false

# Omit cluster for a standalone server.
# cluster:
#   mode: master
#   slaves: ["ns2.example.com"]

database:
  type: "postgres"               # sqlite uses a local file; other values use PostgreSQL
  sqlite_path: "queries.db"        # Relative to data-dir
  host: "127.0.0.1:5432"
  user: "dns"
  password: "replace-this-password"
  db_name: "dns_db"

external_api:
  geo_ip_api_key: "none"
```

`192.0.2.0/24` and `2001:db8::/32` are documentation ranges; replace them with real addresses. Add country branches only when Geo routing is needed. When a branch such as `JP` matches, missing record types are **not** inherited from `default`.

## `server`: listeners and GeoIP

| Field | Default | Description |
| --- | --- | --- |
| `listen` | Local active non-loopback addresses on port 53 | List of IPv4 or IPv6 UDP/TCP addresses. Each entry gets both sockets. The generated list is a snapshot; edit it after network address changes. |
| `enable_geoip_mmap` | `false` | `false`: compact Go heap index; `true`: file-mapped compact index. Requires restart. |
| `geoip_update_url` | empty | Direct HTTP(S) URL for the GeoIP data file; empty disables automatic updates. |
| `geoip_update_interval` | `24h` when a URL is set | Positive Go duration such as `12h` or `48h`. |

Older configurations with a scalar `listen` value must change it to a list. Put any former `listen_ipv6` address in the same list. The generated list is only created for a missing config file; it does not update automatically when interfaces change.

Geo lookup order is: existing IPv4 `/24` in-memory cache → `geo_cache.db` SQLite cache → local `geoip.dat` → API fallback. `geoip.dat` is read from the data directory (`/var/lib/calidns` for packages).

Commercial integrations live under `external_api`. The core `geo.Provider` accepts an IP and returns nullable `CountryCode`, `SubLocation`, `ASN`, and `ASNName` values. The bundled ip2location provider reads `external_api.geo_ip_api_key`; an empty or `none` key disables it.

With `enable_geoip_mmap: true`, startup creates a temporary compact index beside `geoip.dat`, maps it, and unlinks the temporary file. That directory must be writable. Without mmap, the process retains only the compact lookup index in Go memory, not the original `geoip.dat` contents.

When `geoip_update_url` is set, the next update is due at **`geoip.dat` mtime + `geoip_update_interval`**. A missing or overdue file triggers an immediate download at startup; otherwise the server waits for the remaining interval. The URL must serve a raw `geoip.dat` file, not a ZIP, HTML page, or API JSON. A replacement is validated and indexed before the on-disk file and live index are switched. Failed updates keep the old data and retry after a backoff from one minute to one hour. A successful update sets the file mtime to the update time. Existing SQLite `/24` cache entries take precedence until they expire or are deleted. Manually replacing `geoip.dat` still requires a restart.

Use a trusted HTTPS source. Automatic updates require write access to the config directory, and briefly holding both old and new indexes may increase memory use during replacement.

## `server.api`: management API

| Field | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Start the HTTP API. |
| `listen` | `:3101` | HTTP listen address. |
| `tokens` | empty | Bearer tokens. **An empty list disables authentication**; set tokens in production. |
| `cors.allow_origins` | `["*"]` | Allowed origins; omitted means all origins. |
| `cors.allow_methods` | GET, POST, PUT, DELETE, OPTIONS, HEAD, PATCH | Allowed methods. |
| `cors.allow_headers` | Content-Type, Authorization, X-Requested-With, Accept, Origin | Allowed request headers. |
| `cors.expose_headers` | empty | Response headers exposed to browsers. |
| `cors.max_age` | `0` | CORS preflight cache lifetime in seconds. |
| `cors.allow_credentials` | `false` | Whether to emit the CORS credentials header. |

See the [API reference](API_EN.md) for paths and examples. `GET /api/server` displays GeoIP mmap and update settings, but `PUT /api/server` does not hot-swap them; edit YAML and restart for those settings.

## `database` and `cluster`

`database` stores DNS query and EDNS history. In SQLite mode, a relative `sqlite_path` is resolved under the data directory, producing `/var/lib/calidns/queries.db` for package installs by default.

Any other `type` value, or no `type`, uses PostgreSQL. `geo_cache.db` is a separate SQLite Geo cache in the data directory.

Omit `cluster` for standalone operation. With `mode: master`, a node forwards API Zone and some server-setting changes to `slaves`. With `mode: slave` and `master` set, a node fetches zones and business settings from its master's `/api/config` at startup. Listener addresses, database settings, cluster addresses, and GeoIP mmap/update settings remain node-local; do not assume they are hot-synced. Nodes need reachable APIs and appropriate tokens.

## `dns_data.db`: defaults and zones

First startup creates one `dns_defaults` row with TTL 300, recording disabled, and `refuse`, plus the `dns_zones` table. Both are managed through the Web API.

Each zone can select how its key is matched. `mode: simple` treats the key as an exact DNS name, case-insensitively and with an optional trailing dot. `mode: golang` treats the key as a Go regular expression, for example `'^www\.example\.com\.?$'`. When `mode` is omitted, the legacy behavior remains: plain names are exact and keys containing regex metacharacters are treated as regular expressions. Avoid overlapping patterns because zone iteration uses a map and match precedence is not guaranteed.

| Zone field | Description |
| --- | --- |
| `mode` | `simple` for an exact domain or `golang` for a Go regular expression; omitted preserves legacy auto-detection. |
| `default` | Fallback record set when no country branch matches; normally provide one. |
| `JP`, `US`, etc. | Complete record set for that country; **does not merge missing record types from `default`**. |
| `ttl` | Zone TTL in seconds; omitted uses the database `default_ttl`. |
| `record` | Overrides the database `default_record`. |
| `fast_open` | Always selects `default` instead of Geo routing; Geo lookup may still be used for logging. |

Unmatched zones use the database `default_response`. A matched zone without the requested type in its selected branch returns NOERROR/NODATA with a generated SOA. Failed or missing Geo lookup selects `default`.

Each record field is an array of strings:

| Field | Example | Description |
| --- | --- | --- |
| `a` | `"192.0.2.10"` | IPv4 address. |
| `aaaa` | `"2001:db8::10"` | IPv6 address. |
| `txt` | `"v=spf1 mx -all"` | One TXT string. |
| `cname` | `"target.example.com."` | Alias target. |
| `mx` | `"10 mail.example.com."` | Priority and mail server. |
| `ns` | `"ns1.example.com."` | Name server. |
| `srv` | `"10 5 443 service.example.com."` | Priority, weight, port, target. |
| `caa` | `'0 issue "letsencrypt.org"'` | Flags, tag, value. |
| `ptr` | `"host.example.com."` | Reverse target; match the relevant `in-addr.arpa` or `ip6.arpa` name yourself. |
| `soa` | `"ns1.example.com. hostmaster.example.com. 2026091601 3600 900 1209600 300"` | MNAME, RNAME, serial, refresh, retry, expire, minimum. |
| `other` | `"SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"` | Other supported RR types as `TYPE RDATA`, including TLSA. |

Use fully qualified domain targets ending in `.`. Do not include the owner name, TTL, or `IN` in `other`; the server supplies those. The API validates new records. Invalid manually edited YAML records are skipped when queried and logged. Currently `cname` answers only CNAME queries; it does not automatically turn A/AAAA requests into alias responses. `ns` returns NS records but does not generate glue records.

See the [Zone API](API_EN.md#zones) for record management.
