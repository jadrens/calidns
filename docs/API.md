# DNS Server API 文档

[README](../README.md) · [English](API_EN.md) · [配置文档](CONFIG.md)

YAML 字段和完整配置示例见 [配置文档](CONFIG.md)。

## 概述

- **默认端口**: `3101`（可在当前配置文件的 `server.api.listen` 中配置）
- **Content-Type**: `application/json`
- **CORS**: 已启用（允许所有来源）

GeoIP 索引模式由当前配置文件的 `server.geoip.enable_mmap` 控制，修改后需重启。`false` 使用常驻 Go 内存中的紧凑索引；`true` 使用文件映射的紧凑索引，并要求 `geoip.dat` 所在目录可写，以便启动时创建临时索引文件。
自动更新由 `server.geoip.update_url` 和 `server.geoip.update_interval` 控制，更新时间按 `geoip.dat` 的 mtime 计算，详见 [配置文档](CONFIG.md)。
`GET /api/server` 会返回分组的 `geoip` 和 `dashboard` 对象；`dashboard.url` 在内置和远程 ZIP 模式下均为当前请求 host 加 `/dashboard`。`PUT /api/server` 不会热切换这些设置。

## 鉴权

除 `/api/health` 外，所有 API 端点均需 **Bearer Token** 鉴权。

在当前配置文件中配置 token 列表:

```yaml
server:
  api:
    enabled: true
    listen: ":3101"
    tokens:
      - "your-secret-token-here"
      - "another-backup-token"
```

- 请求时在 `Authorization` 头中携带 `Bearer <token>`
- 多个 token 均有效，可用于轮换
- 若 `tokens` 为空数组，则不验证（开发模式，生产环境务必配置）

---

## 端点列表

### 1. 健康检查

> ⚠️ 无需鉴权

**GET** `/api/health`

**响应示例**:
```json
{
    "status": "ok"
}
```

---

### 2. 服务器统计

**GET** `/api/stats`

**响应示例**:
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
> `cache_hited` = 缓存命中记录数，`total_queries` = 总记录数，`dropped` = 入库失败的丢弃记录数。若 recorder 未启用，`recorder` 返回 `{"enabled": false}`

---

### 3. 查看 / 修改服务器默认配置

**GET** `/api/server`

获取当前服务器默认参数。

**响应示例**:
```json
{
    "listen": ["192.0.2.42:53", "[2001:db8::42]:53"],
    "default_ttl": 300,
    "default_response": "refuse",
    "default_record": false,
    "geoip": {
        "enable_mmap": false,
        "update_url": "https://cdn.jsdelivr.net/gh/v2fly/geoip@release/geoip.dat",
        "update_interval": "24h"
    },
    "dashboard": {
        "url": "https://dns.example.com/dashboard",
        "update_interval": "24h"
    }
}
```

**字段说明**:
| 字段 | 类型 | 说明 |
|------|------|------|
| `listen` | string | DNS 服务器 UDP 监听地址（只读） |
| `default_ttl` | int | 默认 TTL（秒），当 zone 未指定 ttl 时使用 |
| `default_response` | string | 未匹配 zone 时的默认响应：`refuse`、`nxdomain`、`servfail` |
| `default_record` | bool | 是否默认记录查询日志 |
| `geoip.enable_mmap` | bool | GeoIP 索引是否使用 mmap；只读，编辑 YAML 后重启 |
| `geoip.update_url` | string | GeoIP 自动更新地址；只读，编辑 YAML 后重启 |
| `geoip.update_interval` | string | 基于 `geoip.dat` mtime 的更新间隔；只读，编辑 YAML 后重启 |
| `dashboard.url` | string | 本机 `/dashboard` 访问地址；只读，编辑 YAML 后重启 |
| `dashboard.update_interval` | string | 远程 Dashboard ZIP 更新间隔；只读，编辑 YAML 后重启 |

**PUT** `/api/server`

修改服务器默认参数。只需传要修改的字段，即时生效无需重启。

**请求体**（所有字段可选）:
```json
{
    "default_ttl": 600,
    "default_response": "nxdomain",
    "default_record": true
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `default_ttl` | int | 否 | 默认 TTL（秒），必须 > 0 |
| `default_response` | string | 否 | `refuse`、`nxdomain` 或 `servfail` |
| `default_record` | bool | 否 | 是否默认记录查询日志 |

**响应成功** (200):
```json
{"status": "ok"}
```

**响应失败** (400):
```json
{"error": "default_response must be one of: refuse, nxdomain, servfail"}
```

> 💡 修改后会自动保存到数据目录的 `dns_data.db`（软件包默认为 `/var/lib/calidns/dns_data.db`）；`config.yaml` 不会被改写。

---

### 4. 列出所有 Zone

**GET** `/api/zones`

**响应示例**:
```json
{
    "total": 2,
    "zones": [
        {
            "pattern": "^aaa\\.bbb\\.com\\.?$",
            "regex": "^aaa\\.bbb\\.com\\.?$",
            "mode": "golang",
            "countries": {
                "default": {
                    "a": ["1.1.1.1", "2.2.2.2"],
                    "aaaa": ["1234::", "5678::"]
                },
                "CN": {
                    "a": ["6.6.6.6", "7.7.7.7"],
                    "txt": ["hello from CN"]
                }
            },
            "ttl": 600,
            "record": true,
            "fast_open": false
        }
    ]
}
```

---

### 5. 获取单个 Zone

**GET** `/api/zones?pattern=<正则表达式>`

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | Zone 的正则表达式（需 URL 编码） |

**请求示例**:
```
GET /api/zones?pattern=%5Eaaa%5C%5C.bbb%5C%5C.com%5C%5C.%3F%24
```

**响应成功** (200):
```json
{
    "pattern": "^aaa\\.bbb\\.com\\.?$",
    "regex": "^aaa\\.bbb\\.com\\.?$",
    "mode": "golang",
    "countries": {
        "CN": {
            "a": ["6.6.6.6", "7.7.7.7"],
            "txt": ["hello from CN"]
        },
        "default": {
            "a": ["1.1.1.1", "2.2.2.2"],
            "aaaa": ["1234::", "5678::"]
        }
    },
    "ttl": 600,
    "record": true,
    "fast_open": false
}
```

**响应失败** (404):
```json
{"error": "zone not found"}
```

---

### 6. 新增 / 更新 Zone

**POST** `/api/zones`

> 若 pattern 已存在则更新，否则新增。即时生效，无需重启。

**请求体**:
```json
{
    "pattern": "^test\\.dns\\.com\\.?$",
    "mode": "golang",
    "countries": {
        "default": {
            "a": ["99.99.99.99"],
            "txt": ["default text"],
            "mx": ["10 mail.example.com."],
            "ns": ["ns1.example.com."],
            "caa": ["0 issue \"letsencrypt.org\""],
            "other": ["SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"]
        },
        "JP": {
            "a": ["1.2.3.4"],
            "cname": ["jp.example.com"]
        },
        "US": {
            "a": ["8.8.8.8"]
        }
    },
    "ttl": 300,
    "record": true,
    "fast_open": false
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | Go 正则表达式，用于匹配域名 |
| `mode` | string | 否 | `simple` 为精确域名匹配，`golang` 为 Go 正则；省略时沿用旧版自动判断 |
| `countries` | object | 否 | 国家代号 → 记录集的映射，`"default"` 为兜底 |
| `countries.<code>.a` | []string | 否 | A 记录（IPv4） |
| `countries.<code>.aaaa` | []string | 否 | AAAA 记录（IPv6） |
| `countries.<code>.txt` | []string | 否 | TXT 记录 |
| `countries.<code>.cname` | []string | 否 | CNAME 记录 |
| `countries.<code>.mx` | []string | 否 | MX RDATA，如 `10 mail.example.com.` |
| `countries.<code>.ns` | []string | 否 | NS 目标域名，如 `ns1.example.com.` |
| `countries.<code>.srv` | []string | 否 | SRV RDATA，如 `10 5 443 service.example.com.` |
| `countries.<code>.caa` | []string | 否 | CAA RDATA，如 `0 issue "letsencrypt.org"` |
| `countries.<code>.ptr` | []string | 否 | PTR 目标域名，如 `host.example.com.` |
| `countries.<code>.soa` | []string | 否 | SOA RDATA：主 NS、管理员邮箱、serial、refresh、retry、expire、minimum |
| `countries.<code>.other` | []string | 否 | 其他 RR：`类型 RDATA`，如 `SSHFP 1 1 <40位十六进制>`、`TLSA 3 1 1 <摘要>` |
| `ttl` | int | 否 | 该 zone 的 TTL（秒），不填则使用服务器默认值 |
| `record` | bool | 否 | 是否记录查询日志到 `database` 配置的 SQLite 或 PostgreSQL |
| `fast_open` | bool | 否 | 开启后跳过 geo 查询，直接返回 `default` 记录，日志中国家代号为 `OO` |

上述记录字段均保存到 `dns_data.db`。域名目标建议写为以 `.` 结尾的完整域名；`other` 使用标准 DNS zone-file 的“类型 + RDATA”格式，不包含记录名、TTL 或 `IN`。API 会拒绝格式错误的新增记录；未配置相应查询类型时仍返回 NODATA。

**响应成功** (200):
```json
{
    "status": "ok",
    "pattern": "^test\\.dns\\.com\\.?$"
}
```
**响应失败** (400):
```json
{"error": "invalid regex pattern"}
```

---

### 7. 删除整个 Zone

**DELETE** `/api/zones?pattern=<正则表达式>`

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | Zone 的正则表达式（需 URL 编码） |

**请求示例**:
```
DELETE /api/zones?pattern=%5Etest%5C%5C.dns%5C%5C.com%5C%5C.%3F%24
```

**响应成功** (200):
```json
{
    "status": "deleted",
    "pattern": "^test\\.dns\\.com\\.?$"
}
```

**响应失败** (404):
```json
{"error": "zone not found"}
```

---

### 8. 删除 Zone 中的指定国家

**DELETE** `/api/zones?pattern=<正则表达式>&country=<国家代号>`

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | Zone 的正则表达式（需 URL 编码） |
| `country` | string | 是 | 国家代号（如 `CN`、`US`）。`default` 不可删除 |

**请求示例**:
```
DELETE /api/zones?pattern=%5Etest%5C%5C.dns%5C%5C.com%5C%5C.%3F%24&country=JP
```

**响应成功** (200):
```json
{
    "status": "deleted",
    "pattern": "^test\\.dns\\.com\\.?$",
    "country": "JP"
}
```

**响应失败** (404):
```json
{"error": "zone or country not found (default cannot be deleted)"}
```

---

### 9. 查询 DNS 历史记录

**GET** `/api/queries`

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `id` | int | - | 按记录 ID 精确查找（可选） |
| `domain` | string | - | 按域名关键字筛选（模糊匹配，可选） |
| `country_code` | string | - | 按客户端 IP 国家代码筛选（可选） |
| `ip` | string | - | 按客户端 IP 关键字筛选（模糊匹配，可选） |
| `subnet` | string | - | 按客户端 IP /24 网段筛选（如 `203.0.113.0/24`）（可选） |
| `start` | string | - | 起始时间，格式 `2006-01-02 15:04:05` 或 `2006-01-02T15:04:05`（可选） |
| `end` | string | - | 结束时间（可选） |
| `limit` | int | `50` | 每页条数（最大 `1000`） |
| `offset` | int | `0` | 偏移量，用于分页 |

> 所有参数均为可选，不加任何参数返回全部记录。

**请求示例**:
```
GET /api/queries?domain=aaa.bbb.com&start=2026-07-01T00:00:00&end=2026-07-04T23:59:59&limit=10&offset=0
GET /api/queries?country_code=CN&limit=20
GET /api/queries?ip=203.0.113.5
GET /api/queries?subnet=203.0.113.0/24
```

**返回字段说明**:

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | int | 记录 ID |
| `domain` | string | 查询域名 |
| `query_type` | string | DNS 查询类型 |
| `client_ip` | string | 客户端 IP |
| `country_code` | string | 客户端 IP 国家代码 |
| `city` | string | 客户端 IP 城市 |
| `geo_cached` | bool | 该次 Geo IP 查询是否命中缓存 |
| `asn` | string | 客户端 IP 的 AS 编号 |
| `as_name` | string | 客户端 IP 的 AS 名称/运营商 |
| `edns_subnet` | string | EDNS Client Subnet 网段（仅当请求携带时） |
| `edns_country_code` | string | EDNS 网段的国家代码 |
| `edns_city` | string | EDNS 网段的城市 |
| `edns_asn` | string | EDNS 网段的 AS 编号 |
| `edns_as_name` | string | EDNS 网段的 AS 名称 |
| `nsid` | string | Name Server Identifier |
| `created_at` | string | 记录时间 |

**响应示例**:
```json
{
    "total": 142,
    "items": [
        {
            "id": 42,
            "domain": "aaa.bbb.com",
            "query_type": "AAAA",
            "client_ip": "203.0.113.5",
            "country_code": "CN",
            "city": "Shanghai",
            "geo_cached": false,
            "asn": "13335",
            "as_name": "CloudFlare Inc",
            "edns_subnet": "1.2.3.0/24",
            "edns_country_code": "US",
            "edns_city": "New York",
            "edns_asn": "15169",
            "edns_as_name": "Google LLC",
            "nsid": "ns1.example.net",
            "created_at": "2026-07-04T08:23:30.799900446Z"
        },
        {
            "id": 43,
            "domain": "aaa.bbb.com",
            "query_type": "A",
            "client_ip": "203.0.113.5",
            "country_code": "CN",
            "city": "Shanghai",
            "geo_cached": true,
            "asn": "4134",
            "as_name": "China Telecom",
            "edns_subnet": "",
            "edns_country_code": "",
            "edns_city": "",
            "edns_asn": "",
            "edns_as_name": "",
            "nsid": "",
            "created_at": "2026-07-04T08:23:30.786788665Z"
        }
    ]
}
```
> `total` 为符合条件的总记录数。`edns_subnet`、`edns_country_code`、`nsid` 仅在请求携带 EDNS 选项时有值。若 recorder 未启用，返回 503。

---

### 10. 删除历史记录（统一删除）

**DELETE** `/api/queries`

支持与查询端点相同的过滤参数，不加任何参数 = 删除全部记录。⚠️ 谨慎操作，不可恢复。

| 参数 | 类型 | 说明 |
|------|------|------|
| `id` | int | 按记录 ID 删除（可选） |
| `domain` | string | 按域名删除（可选） |
| `country_code` | string | 按国家代码删除（可选） |
| `ip` | string | 按精确客户端 IP 删除（可选） |
| `subnet` | string | 按客户端 IP /24 网段删除（如 `203.0.113.0/24`）（可选） |
| `start` | string | 删除起始时间之前的记录（可选） |
| `end` | string | 删除结束时间之后的记录（可选） |

> 所有参数均为可选。多个参数之间为 AND 关系。**不加任何参数删除全部记录。**

**请求示例**:
```
DELETE /api/queries?domain=aaa.bbb.com
DELETE /api/queries?country_code=CN
DELETE /api/queries?ip=203.0.113.5
DELETE /api/queries?subnet=203.0.113.0/24
DELETE /api/queries   # ⚠️ 删除全部
```

**响应成功** (200):
```json
{
    "status": "deleted",
    "deleted": 142
}
```

---

### 11. 查看 Geo 缓存

**GET** `/api/geo-cache`

列出所有有效的 Geo IP 缓存条目（按 /24 网段聚合，已过期条目自动排除）。

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `country_code` | string | - | 按国家代码筛选（可选，如 `US`） |
| `limit` | int | `50` | 每页条数（最大 `1000`） |
| `offset` | int | `0` | 偏移量，用于分页 |

**请求示例**:
```
GET /api/geo-cache
GET /api/geo-cache?country_code=US
GET /api/geo-cache?country_code=CN&limit=10&offset=0
```

**响应示例**:
```json
{
    "total": 3,
    "entries": [
        {
            "subnet": "1.2.3.0/24",
            "country_code": "US",
            "city": "New York",
            "asn": "13335",
            "as_name": "CloudFlare Inc",
            "expires_at": "2026-07-11T12:00:00Z"
        },
        {
            "subnet": "8.8.8.0/24",
            "country_code": "US",
            "city": "Mountain View",
            "asn": "15169",
            "as_name": "Google LLC",
            "expires_at": "2026-07-11T08:30:00Z"
        },
        {
            "subnet": "203.0.113.0/24",
            "country_code": "CN",
            "city": "Shanghai",
            "asn": "4134",
            "as_name": "China Telecom",
            "expires_at": "2026-07-11T04:15:00Z"
        }
    ]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `subnet` | string | /24 网段 CIDR 表示 |
| `country_code` | string | ISO 3166-1 国家代号 |
| `city` | string | 城市名 |
| `asn` | string | AS 编号 |
| `as_name` | string | AS 名称/运营商 |
| `expires_at` | string | 过期时间（RFC3339），7 天后自动删除 |

---

### 12. 删除 Geo 缓存

**DELETE** `/api/geo-cache`

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `subnet` | string | 否 | /24 网段 CIDR（如 `1.2.3.0/24`） |
| `country_code` | string | 否 | 按国家代码删除（如 `US`） |

> 不传任何参数 = 删除全部缓存。`subnet` 和 `country_code` 可组合使用。

**请求示例**:
```
DELETE /api/geo-cache?subnet=1.2.3.0/24
DELETE /api/geo-cache?country_code=US
DELETE /api/geo-cache?subnet=1.2.3.0/24&country_code=US
DELETE /api/geo-cache   # 删除全部
```

**响应成功** (200):
```json
{
    "status": "deleted",
    "subnet": "1.2.3.0/24",
    "country_code": "",
    "deleted": 1
}
```

删除全部时 `subnet` 为空字符串，`deleted` 为总删除数。

> 删除操作同时清除 SQLite 和内存缓存，下次查询该网段将重新调用外部 API。

---

### 13. EDNS 记录

**GET** `/api/edns`

查询带有 EDNS Client Subnet (ECS) 的 DNS 请求记录（仅返回请求中携带了 EDNS 选项的记录）。同时返回该请求的主查询信息和 EDNS 扩展信息，通过 `id` 关联。

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `id` | int | - | 按记录 ID 精确查找（可选） |
| `subnet` | string | - | 按 EDNS 子网 CIDR 精确匹配（可选，如 `1.2.3.0/24`） |
| `country_code` | string | - | 按 EDNS 子网的国家代码过滤（可选） |
| `nsid` | string | - | 按 NSID 过滤（可选） |
| `start` | string | - | 起始时间（可选） |
| `end` | string | - | 结束时间（可选） |
| `limit` | int | `50` | 每页条数（最大 `1000`） |
| `offset` | int | `0` | 偏移量，用于分页 |

**请求示例**:
```
GET /api/edns?country_code=US&limit=10&offset=0
GET /api/edns?id=42
GET /api/edns?subnet=1.2.3.0/24
GET /api/edns?start=2026-07-01T00:00:00&end=2026-07-04T23:59:59
```

**响应示例**:
```json
{
    "total": 3,
    "items": [
        {
            "id": 42,
            "domain": "example.com",
            "query_type": "A",
            "client_ip": "203.0.113.5",
            "country_code": "CN",
            "city": "Shanghai",
            "subnet": "1.2.3.0/24",
            "edns_country_code": "US",
            "edns_city": "New York",
            "edns_asn": "15169",
            "edns_as_name": "Google LLC",
            "nsid": "ns1.example.net",
            "created_at": "2026-07-04T08:23:30.799900446Z"
        }
    ]
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | int | 记录 ID（与 `queries` 表 `id` 一致） |
| `domain` | string | 查询域名 |
| `query_type` | string | DNS 查询类型（A / AAAA / TXT / CNAME） |
| `client_ip` | string | 客户端 IP |
| `country_code` | string | 客户端 IP 地理位置国家代码 |
| `city` | string | 客户端 IP 地理位置城市 |
| `subnet` | string | EDNS Client Subnet 网段（CIDR 格式） |
| `edns_country_code` | string | EDNS 网段地理位置国家代码 |
| `edns_city` | string | EDNS 网段地理位置城市 |
| `edns_asn` | string | EDNS 网段 AS 编号 |
| `edns_as_name` | string | EDNS 网段 AS 名称/运营商 |
| `nsid` | string | Name Server Identifier |
| `created_at` | string | 记录时间（RFC3339） |

> 关联方式: `edns` 表的 `id` 与 `queries` 表的 `id` 一一对应。没有携带 EDNS 选项的请求不会出现在此端点中。

**DELETE** `/api/edns`

删除 EDNS 记录（仅删除 `edns` 表，不删除对应的 `queries` 主记录）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `id` | int | 按记录 ID 删除（可选） |
| `subnet` | string | 按 EDNS 子网删除（可选，如 `1.2.3.0/24`） |
| `country_code` | string | 按 EDNS 子网的国家代码删除（可选） |
| `nsid` | string | 按 NSID 删除（可选） |
| `start` | string | 删除起始时间之前的记录（可选） |
| `end` | string | 删除结束时间之后的记录（可选） |

> 所有参数均为可选。不加任何参数 = 删除全部 EDNS 记录。仅删除 `edns` 表，不影响 `queries` 主表。

**请求示例**:
```
DELETE /api/edns?country_code=US
DELETE /api/edns?subnet=1.2.3.0/24
DELETE /api/edns?id=42
DELETE /api/edns   # ⚠️ 删除全部 EDNS 记录
```

**响应成功** (200):
```json
{
    "status": "deleted",
    "deleted": 3
}
```

---

## 错误码

| HTTP 状态码 | 说明 |
|-------------|------|
| `200` | 成功 |
| `400` | 请求参数无效 |
| `401` | 未鉴权（token 缺失或无效） |
| `404` | 资源不存在（zone 未找到） |
| `405` | 方法不允许 |
| `503` | Recorder 未启用 |

所有错误响应均采用统一格式:
```json
{"error": "错误描述信息"}
```

---

## 使用示例 (curl)

```bash
TOKEN="your-secret-token-here"

# 健康检查（无需鉴权）
curl http://localhost:3101/api/health

# 查看 / 修改服务器默认配置
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/server
curl -X PUT http://localhost:3101/api/server \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"default_ttl": 600, "default_response": "nxdomain"}'

# 查看所有 zone
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/zones

# 获取指定 zone（注意 URL 编码）
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/zones?pattern=%5Eaaa%5C%5C.bbb%5C%5C.com%5C%5C.%3F%24"

# 新增 zone
curl -X POST http://localhost:3101/api/zones \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "pattern": "^new\\.domain\\.com\\.?$",
    "countries": {
      "default": {"a": ["10.0.0.1"], "txt": ["hello"]},
      "CN": {"a": ["1.2.3.4"]}
    },
    "ttl": 600,
    "record": true
  }'

# 删除 zone 中的国家
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/zones?pattern=%5Etest%5C%5C.dns%5C%5C.com%5C%5C.%3F%24&country=JP"

# 删除整个 zone
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/zones?pattern=%5Etest%5C%5C.dns%5C%5C.com%5C%5C.%3F%24"

# 查询历史记录（分页 + 时间范围）
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?domain=aaa.bbb.com&limit=20&offset=0"

# 按国家、IP、网段筛选查询
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?country_code=CN&limit=10"
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?ip=203.0.113.5"
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?subnet=203.0.113.0/24"

# 删除记录（与查询相同的过滤参数）
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?domain=aaa.bbb.com"
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?country_code=CN"
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/queries?subnet=203.0.113.0/24"
# ⚠️ 以下删除全部记录
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:3101/api/queries

# 查看 geo 缓存（可选按国家筛选）
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/geo-cache
curl -H "Authorization: Bearer $TOKEN" "http://localhost:3101/api/geo-cache?country_code=US"

# 删除 geo 缓存（按网段 / 国家 / 组合）
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/geo-cache?subnet=1.2.3.0/24"
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/geo-cache?country_code=US"
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:3101/api/geo-cache

# 查询 EDNS 记录（分页）
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?limit=20&offset=0"

# 按国家过滤 EDNS 记录
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?country_code=US"

# 按 EDNS 网段过滤
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?subnet=1.2.3.0/24"

# 按记录 ID 查询 EDNS
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?id=42"

# 删除 EDNS 记录（与查询相同过滤参数）
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?country_code=US"
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  "http://localhost:3101/api/edns?subnet=1.2.3.0/24"
# ⚠️ 以下删除全部 EDNS 记录
curl -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:3101/api/edns

# 查看统计
curl -H "Authorization: Bearer $TOKEN" http://localhost:3101/api/stats

# 测试未鉴权（应返回 401）
curl http://localhost:3101/api/zones
```
