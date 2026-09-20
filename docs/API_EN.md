# DNS Server HTTP API

[README](../README.md) · [Configuration guide](CONFIG_EN.md) · [中文](API.md)

## Overview and authentication

The API is disabled by default. Enable it with `server.api.enabled: true` in the active config file; its default listen address is `:3101`. Requests and responses use JSON. CORS is enabled and configurable in `server.api.cors`.

All API endpoints except `GET /api/health` require an `Authorization: Bearer <token>` header **when tokens are configured**. The optional `/dashboard` static UI is public, while its API requests remain authenticated. If `server.api.tokens` is empty, authentication is disabled; do not expose such an API publicly. `OPTIONS` preflight requests return `204` without authentication.

```yaml
server:
  api:
    enabled: true
    listen: ":3101"
    tokens:
      - "replace-with-a-long-random-token"
```

GeoIP and dashboard source/update settings are set in YAML, not hot-switched by the API. `GET /api/server` shows them as grouped objects; edit YAML and restart to change them. See the [configuration guide](CONFIG_EN.md).

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/health` | Public health check |
| GET | `/api/stats` | Zone and recorder statistics |
| GET, PUT | `/api/server` | Read or change selected server defaults |
| GET, POST, PUT, DELETE | `/api/zones` | Manage zones and country branches |
| GET, DELETE | `/api/queries` | Query or delete DNS history |
| GET, DELETE | `/api/geo-cache` | Query or delete Geo cache entries |
| GET, DELETE | `/api/edns` | Query or delete EDNS history |
| GET | `/api/config` | Export configuration for cluster sync |

### Health and statistics

`GET /api/health` is public and returns `{"status":"ok"}`.

`GET /api/stats` returns the zone count and recorder counters:

```json
{
  "zones": 2,
  "recorder": {
    "queue_len": 0,
    "total_queries": 128,
    "cache_hited": 121,
    "dropped": 0
  }
}
```

`cache_hited` is the number of recorded cache-hit lookups; `dropped` counts failed recorder inserts. If no recorder is attached, `recorder` is `{"enabled":false}`.

### Server defaults

`GET /api/server` returns the current `listen`, `default_ttl`, `default_response`, and `default_record`, plus `geoip` and `dashboard` objects. `dashboard.url` is the request host plus `/dashboard` for either the embedded or remote-ZIP mode; `dashboard.update_interval` reports the configured remote refresh interval.

`PUT /api/server` accepts any subset of these writable fields:

| JSON field | Type | Constraint / effect |
| --- | --- | --- |
| `default_ttl` | integer | Must be greater than zero; updates the resolver immediately. |
| `default_response` | string | `refuse`, `nxdomain`, or `servfail`. |
| `default_record` | boolean | Enables or disables default query recording. |

```json
{
  "default_ttl": 600,
  "default_response": "nxdomain",
  "default_record": true
}
```

Success returns `{"status":"ok"}`. Invalid values or a body with no writable fields return `400`. Accepted changes are saved to the data directory's `dns_data.db` (`/var/lib/calidns/dns_data.db` for package installs) and forwarded by a master to configured slaves. `config.yaml` is not rewritten.

### Zones

`GET /api/zones` returns `{"total": N, "zones": [...]}`. Supply `?pattern=<url-encoded-pattern>` to get one zone; a missing pattern returns `404`. Each zone includes `pattern`, `regex`, `mode`, `countries`, `ttl`, `record`, and `fast_open`.

`POST /api/zones` and `PUT /api/zones` both upsert a zone. A new pattern creates a zone; an existing pattern replaces it. Changes take effect immediately and are saved. Example:

```json
{
  "pattern": "example.com",
  "mode": "simple",
  "countries": {
    "default": {
      "a": ["192.0.2.10"],
      "aaaa": ["2001:db8::10"],
      "mx": ["10 mail.example.com."],
      "ns": ["ns1.example.com."],
      "other": ["SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"]
    },
    "JP": {
      "a": ["192.0.2.20"]
    }
  },
  "ttl": 300,
  "record": true,
  "fast_open": false
}
```

| Field | Type | Description |
| --- | --- | --- |
| `pattern` | string | Required domain name or Go regular expression. |
| `mode` | string | `simple` matches `pattern` as an exact DNS name; `golang` uses it as a Go regular expression. Omit for legacy auto-detection. |
| `countries` | object | Country code to record-set mapping; `default` is the fallback. |
| `ttl` | integer | Zone TTL in seconds; omitted uses the server default. |
| `record` | boolean | Record queries for this zone. |
| `fast_open` | boolean | Use `default` without Geo routing; recorded country code is `OO`. |

Every record-set field is an array of strings:

| Field | Example / meaning |
| --- | --- |
| `a` | `"192.0.2.10"` — IPv4 address |
| `aaaa` | `"2001:db8::10"` — IPv6 address |
| `txt` | `"v=spf1 mx -all"` — TXT string |
| `cname` | `"target.example.com."` — alias target |
| `mx` | `"10 mail.example.com."` — priority and mail server |
| `ns` | `"ns1.example.com."` — name server |
| `srv` | `"10 5 443 service.example.com."` — priority, weight, port, target |
| `caa` | `'0 issue "letsencrypt.org"'` — flags, tag, value |
| `ptr` | `"host.example.com."` — reverse target |
| `soa` | `"ns1.example.com. hostmaster.example.com. 2026091601 3600 900 1209600 300"` — SOA RDATA |
| `other` | `"SSHFP 1 1 <hex>"` or `"TLSA 3 1 1 <digest>"` — `TYPE RDATA` |

The API validates record formats and rejects invalid input with `400`. Use fully qualified target names ending in `.`. The `other` value must not include an owner name, TTL, or `IN`. The fields have the same names in YAML. A country branch does not inherit missing record types from `default`.

`DELETE /api/zones?pattern=<url-encoded-pattern>` removes the entire zone. Add `&country=JP` to remove just that branch; `default` cannot be removed this way. Success returns `{"status":"deleted","pattern":"example.com"}` (plus `"country":"JP"` when applicable). Missing zones or countries return `404`; a missing `pattern` returns `400`.

### DNS query history

`GET /api/queries` returns `{"total": N, "items": [...]}`. All filters are optional and combine with AND:

| Query parameter | Description |
| --- | --- |
| `id` | Exact record ID. |
| `domain` | Case-insensitive substring match. |
| `ip` | Client-IP substring match. |
| `subnet` | Client IP contained in the given CIDR, for example `203.0.113.0/24`. |
| `country_code` | Client-IP country code. |
| `start`, `end` | Inclusive creation-time bounds; use RFC3339 timestamps such as `2026-07-01T00:00:00Z`. |
| `limit` | Page size; default `50`, maximum `1000`. |
| `offset` | Pagination offset; default `0`. |

```http
GET /api/queries?domain=example.com&start=2026-07-01T00:00:00Z&limit=10&offset=0
GET /api/queries?country_code=JP&subnet=203.0.113.0/24
```

Each item can contain `id`, `domain`, `query_type`, `client_ip`, `country_code`, `city`, `geo_cached`, `asn`, `as_name`, `created_at`, and EDNS fields `edns_subnet`, `edns_country_code`, `edns_city`, `edns_asn`, `edns_as_name`, and `nsid`. EDNS fields are empty when the query has no corresponding EDNS record.

`DELETE /api/queries` accepts `id`, `domain`, `ip`, `subnet`, `country_code`, `start`, and `end`. Unlike GET, `domain` and `ip` must match exactly. Time bounds select records **within** the inclusive range. Matching query and associated EDNS rows are deleted, and the response is `{"status":"deleted","deleted":N}`.

**Caution:** `DELETE /api/queries` with no filters deletes all query history. Deletion is not recoverable through the API.

### Geo cache

`GET /api/geo-cache` lists unexpired IPv4 `/24` Geo cache entries:

| Query parameter | Description |
| --- | --- |
| `country_code` | Optional country-code filter. |
| `limit` | Positive page size; default `50`. |
| `offset` | Pagination offset; default `0`. |

The response is `{"total":N,"entries":[...]}`. Each entry has `subnet`, `country_code`, `city`, `asn`, `as_name`, and `expires_at` (RFC3339). Example:

```json
{
  "total": 1,
  "entries": [{
    "subnet": "1.2.3.0/24",
    "country_code": "US",
    "city": "New York",
    "asn": "13335",
    "as_name": "Example AS",
    "expires_at": "2026-07-11T12:00:00Z"
  }]
}
```

`DELETE /api/geo-cache` accepts optional `subnet` (a `/24` CIDR) and `country_code` filters, alone or together. It clears matching SQLite and in-memory entries and returns `{"status":"deleted","subnet":"...","country_code":"...","deleted":N}`. A later lookup may use local `geoip.dat` or the external API.

**Caution:** No filters deletes the entire Geo cache.

### EDNS history

`GET /api/edns` returns only queries with an EDNS Client Subnet entry. Its response is `{"total":N,"items":[...]}`, joining each EDNS row to its query by `id`.

| Query parameter | Description |
| --- | --- |
| `id` | Exact record ID. |
| `subnet` | Exact EDNS subnet CIDR. |
| `country_code` | Country code of the EDNS subnet, not the client IP. |
| `nsid` | Exact NSID. |
| `start`, `end` | Inclusive query-time bounds; use RFC3339 timestamps. |
| `limit` | Page size; default `50`, maximum `1000`. |
| `offset` | Pagination offset; default `0`. |

Each item contains query fields `id`, `domain`, `query_type`, `client_ip`, `country_code`, `city`, `created_at`, plus `subnet`, `edns_country_code`, `edns_city`, `edns_asn`, `edns_as_name`, and `nsid`.

`DELETE /api/edns` accepts `id`, `subnet`, `country_code`, `nsid`, `start`, and `end` as filters and returns `{"status":"deleted","deleted":N}`. It deletes only EDNS rows; the main query rows remain. **No filters deletes every EDNS row.**

### Cluster configuration export

`GET /api/config` returns the full configuration for slave startup synchronization. It includes credentials and should be accessible only to trusted peers; it follows normal API authentication rules.

## Errors

Errors use `{"error":"description"}`. Common status codes:

| Status | Meaning |
| --- | --- |
| `200` | Success. |
| `400` | Invalid request or value. |
| `401` | Missing or invalid Bearer token. |
| `404` | Zone or country not found. |
| `405` | Method not allowed. |
| `500` | Database or server error. |
| `503` | Recorder unavailable. |

## curl examples

```sh
TOKEN="your-secret-token-here"

# Public health check
curl http://localhost:3101/api/health

# Read and update server defaults
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/server
curl -X PUT http://localhost:3101/api/server \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"default_ttl":600,"default_response":"nxdomain"}'

# List and create zones
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/zones
curl -X POST http://localhost:3101/api/zones \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"pattern":"example.com","countries":{"default":{"a":["192.0.2.10"]}}}'

# Query history and EDNS records
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?country_code=JP&limit=20"
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?subnet=1.2.3.0/24"

# Inspect and selectively delete Geo cache entries
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/geo-cache
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/geo-cache?subnet=1.2.3.0/24"

# Delete one country branch; URL-encode complex patterns
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/zones?pattern=example.com&country=JP"
```
