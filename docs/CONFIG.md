# DNS Server 配置文档

[README](../README.md) · [English](CONFIG_EN.md) · [API 文档](API.md)

源码入口位于 `src/`，可在项目根目录执行 `mkdir -p local && go build -o local/dns-server ./src` 构建。服务默认读取工作目录下的 `local/config.yaml`；也可以用 `./local/dns-server -config /path/to/config.yaml` 指定路径。指定的文件不存在时，程序会从编入二进制的 [默认模板](../src/config/default_config.yaml) 生成配置（默认使用本地 SQLite）；已有文件不会被覆盖。`local/` 已被 Git 忽略，用于存放本机配置、GeoIP 数据、数据库和构建产物。手动修改 YAML 后需重启服务；通过 [HTTP API](API.md) 更新的 Zone 会即时生效，并写回配置文件。

## 完整示例

以下是可作为起点的配置。请替换数据库连接信息和 API Token，不要直接使用示例值部署。

```yaml
server:
  listen: ":53"
  listen_ipv6: ""                 # 例如 "[::]:53"；留空则不监听 IPv6
  default_ttl: 300
  default_record: false
  default_response: "refuse"     # refuse | nxdomain | servfail
  geo_ip_api_key: "none"          # 可选的 ip2location.io API 兜底 Key
  enable_geoip_mmap: false        # false: Go 内存索引；true: 文件映射索引
  geoip_update_url: ""            # 直链 geoip.dat；空值关闭自动更新
  geoip_update_interval: "24h"    # 以 geoip.dat 的 mtime 计算下次更新时间
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

# 单机部署可省略 cluster。
# cluster:
#   mode: master
#   slaves: ["ns2.example.com"]

database:
  type: "postgres"               # sqlite 使用本地文件；其他值使用 PostgreSQL
  sqlite_path: "queries.db"        # 仅 type=sqlite 时生效
  host: "127.0.0.1:5432"
  user: "dns"
  password: "replace-this-password"
  db_name: "dns_db"

zones:
  example.com:
    default:
      a: ["192.0.2.10"]
      aaaa: ["2001:db8::10"]
      mx: ["10 mail.example.com."]
      ns: ["ns1.example.com.", "ns2.example.com."]
      txt: ["v=spf1 mx -all"]
      caa: ['0 issue "letsencrypt.org"']
      other:
        - "SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"
    JP:
      a: ["192.0.2.20"]
      mx: ["10 mail-jp.example.com."]
    ttl: 300
    record: false
    fast_open: false
```

示例里的 `192.0.2.0/24`、`2001:db8::/32` 是文档地址，部署时要换成实际地址。只有在需要 Geo 分流时才配置国家分支；`JP` 这样的分支一旦命中，不会从 `default` 继承缺少的记录类型。

## `server`：监听、默认应答与 GeoIP

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `:53` | IPv4 UDP/TCP 监听地址。`:53` 表示监听所有 IPv4 地址。 |
| `listen_ipv6` | 空 | IPv6 UDP/TCP 监听地址；例如 `[::]:53`。 |
| `default_ttl` | `300` | Zone 未指定 `ttl` 时的秒数；非正数会使用默认值。 |
| `default_record` | `false` | Zone 未覆盖 `record` 时是否记录 DNS 查询。 |
| `default_response` | `refuse` | 未匹配任何 Zone 时返回 `refuse`、`nxdomain` 或 `servfail`。 |
| `geo_ip_api_key` | `none` | 本地 Geo 数据未命中时使用的 ip2location.io API Key；`none` 只关闭 API 兜底，不关闭本地 Geo。 |
| `enable_geoip_mmap` | `false` | `false` 使用 Go 堆内紧凑索引；`true` 使用文件映射的紧凑索引。重启后生效。 |
| `geoip_update_url` | 空 | GeoIP 数据文件的 HTTP(S) 直接下载地址；空值关闭自动更新。 |
| `geoip_update_interval` | `24h`（配置了 URL 时） | 更新间隔，使用 Go duration 写法，如 `12h`、`48h`；必须大于 0。 |

Geo 查询顺序为：已有的 IPv4 `/24` 内存缓存 → `geo_cache.db` SQLite 缓存 → 本地 `geoip.dat` → API 兜底。`geoip.dat` 应与所使用的配置文件放在同一目录；文件不存在时继续使用缓存/API。它只提供国家代码，不能提供城市或 ASN。IPv6 不使用现有的 `/24` 缓存，会直接查询本地索引或 API。

`enable_geoip_mmap: true` 时，启动过程会在 `geoip.dat` 所在目录创建临时紧凑索引文件，映射成功后删除其目录项；因此该目录必须可写。关闭 mmap 时，运行期间只保留计算用的紧凑索引，不会把原始 `geoip.dat` 常驻内存。

配置 `geoip_update_url` 后，下次更新时间 = **`geoip.dat` 的 mtime + `geoip_update_interval`**。启动时若文件不存在或已经到期，会立即尝试下载；未到期则等待剩余时间。下载内容必须是原始 `geoip.dat` 文件，不能是 ZIP、网页或 API JSON。服务先校验新文件并构建索引，成功后才替换磁盘文件和运行中的索引，无需重启；失败会保留旧文件/索引，并在 1 分钟至 1 小时后重试。成功更新会把文件 mtime 设为更新时间。由于 SQLite `/24` 缓存仍然优先，已有缓存条目在过期或删除前不会立即改用新数据。手工替换 `geoip.dat` 后仍需重启才能加载。

建议使用可信的 HTTPS 地址。自动更新需要配置文件所在目录可写；更新期间会短暂同时保留旧索引与新索引，内存占用可能高于平时。

## `server.api`：HTTP 管理接口

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 是否启动 HTTP API。 |
| `listen` | `:3101` | HTTP API 监听地址。 |
| `tokens` | 空 | Bearer Token 列表。**空列表表示不鉴权**，生产环境必须设置。 |
| `cors.allow_origins` | `["*"]` | 允许的来源；未设置时向所有来源开放。 |
| `cors.allow_methods` | `GET, POST, PUT, DELETE, OPTIONS, HEAD, PATCH` | 允许的 HTTP 方法。 |
| `cors.allow_headers` | `Content-Type, Authorization, X-Requested-With, Accept, Origin` | 允许的请求头。 |
| `cors.expose_headers` | 空 | 暴露给浏览器的响应头。 |
| `cors.max_age` | `0` | CORS 预检缓存秒数。 |
| `cors.allow_credentials` | `false` | 是否返回允许凭据的 CORS 响应头。 |

接口路径、请求与响应示例见 [API 文档](API.md)。`GET /api/server` 可查看 GeoIP mmap 和自动更新配置；`PUT /api/server` 不会热切换这些设置，修改它们需编辑 YAML 并重启。

## `database` 与 `cluster`

`database` 存储 DNS 查询日志和 EDNS 记录；程序启动时会初始化它，即使当前没有开启查询记录也需要可用。`type: sqlite` 适用于单机部署，使用 `sqlite_path` 指定本地数据库文件；相对路径以配置文件所在目录为基准，省略时默认为 `queries.db`。此时 `host`、`user`、`password`、`db_name` 不生效。SQLite 使用 WAL 日志模式；备份时请使用 SQLite 的在线备份方式，或停机后同时处理数据库及其 WAL 文件。

`type` 为其他值或省略时沿用 PostgreSQL；`host` 可写作 `主机:端口`，省略端口时使用 `5432`。从 PostgreSQL 切换到 SQLite 不会自动迁移旧日志。`geo_cache.db` 是独立的 SQLite Geo 缓存文件，位于配置文件所在目录，不受 `database` 字段控制。

`cluster` 可省略。`mode: master` 时，主节点会把通过 API 进行的 Zone/部分服务器配置变更转发给 `slaves`；`mode: slave` 且设置 `master` 时，从节点启动时会从主节点的 `/api/config` 拉取 Zone 和业务配置。`listen`、`listen_ipv6`、数据库、集群地址及 GeoIP mmap/自动更新设置属于节点本地配置，不应假设会随主节点热同步。主从通信依赖各节点的 API 可访问以及相应 Token。

## `zones`：匹配、国家分支和记录

`zones` 的键是域名或 Go 正则表达式。普通域名（如 `example.com`）会自动转为精确匹配；包含正则元字符的键会按原样当作正则表达式使用，例如 `'^www\.example\.com\.?$'`。查询域名会转为小写，建议 Zone 键也使用小写。请避免让多个 Zone 模式匹配同一域名：当前配置用 map 读取，重叠模式的优先顺序不保证固定。

每个 Zone 可设置：

| 字段 | 说明 |
| --- | --- |
| `default` | 国家代码未命中时使用的记录集，建议始终配置。 |
| `JP`、`US` 等 | 对应国家代码的完整记录集；**不会按记录类型与 `default` 合并**。 |
| `ttl` | 该 Zone 的 TTL 秒数；省略时继承 `server.default_ttl`。 |
| `record` | 覆盖 `server.default_record`。 |
| `fast_open` | 为 `true` 时始终选用 `default` 分支，不按 Geo 选择记录；如开启记录日志，Geo 仍可能用于日志。 |

未匹配 Zone 时使用 `server.default_response`。匹配了 Zone、但该国家分支没有所查询的记录类型时，返回 NOERROR/NODATA，并附带程序生成的 SOA。Geo 查询失败或未命中时使用 `default` 分支。

每个国家分支中的记录字段均为字符串数组：

| 字段 | 值示例 | 说明 |
| --- | --- | --- |
| `a` | `"192.0.2.10"` | IPv4 地址。 |
| `aaaa` | `"2001:db8::10"` | IPv6 地址。 |
| `txt` | `"v=spf1 mx -all"` | 一条 TXT 字符串。 |
| `cname` | `"target.example.com."` | 别名目标。 |
| `mx` | `"10 mail.example.com."` | 优先级和邮件服务器。 |
| `ns` | `"ns1.example.com."` | 名称服务器。 |
| `srv` | `"10 5 443 service.example.com."` | 优先级、权重、端口、目标。 |
| `caa` | `'0 issue "letsencrypt.org"'` | Flags、Tag、Value。 |
| `ptr` | `"host.example.com."` | 反向解析目标。Zone 键需自行匹配 `in-addr.arpa` 或 `ip6.arpa` 名称。 |
| `soa` | `"ns1.example.com. hostmaster.example.com. 2026091601 3600 900 1209600 300"` | MNAME、RNAME、Serial、Refresh、Retry、Expire、Minimum。 |
| `other` | `"SSHFP 1 1 0123456789abcdef0123456789abcdef01234567"` | 其他支持的 RR，写作 `类型 RDATA`，也可用于 TLSA 等。 |

域名目标建议使用以 `.` 结尾的完整域名。`other` 中不要写记录名、TTL 或 `IN`，这些由服务按查询域名和 Zone TTL 生成。API 新增 Zone 时会校验新类型的记录格式；手工编辑 YAML 后，格式错误的记录会在对应 DNS 查询时被跳过并写入日志。当前 `cname` 仅在查询 CNAME 类型时返回；不会自动作为 A/AAAA 查询的别名应答。`ns` 提供 NS 记录应答，但不自动生成委派的 glue 记录。

API 与 YAML 的字段名一致；例如 `mx` 在 API 中也是字符串数组。需要动态更新时参见 [Zone API](API.md#6-新增--更新-zone)。
