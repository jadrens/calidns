# DNS Server 配置文档

[README](../README.md) · [English](CONFIG_EN.md) · [API 文档](API.md)

源码入口位于 `src/`。软件包安装时，静态配置位于 `/etc/calidns/config.yaml`，数据库及 GeoIP 数据位于 `/var/lib/calidns/`。源码直接运行时，两者默认使用当前目录；可用 `-config` 和 `-data-dir` 分别指定。Web API 修改的 Zone、`default_ttl`、`default_record`、`default_response` 都写入 `dns_data.db`，不会重写 YAML。不转换旧版 YAML 中的 Zone 和默认项。

## 完整示例

以下是可作为起点的配置。请替换数据库连接信息和 API Token，不要直接使用示例值部署。

```yaml
server:
  listen:
    - "192.0.2.42:53"
    - "[2001:db8::42]:53"
  geoip:
    enable_mmap: false            # false: Go 内存索引；true: 文件映射索引
    update_url: "https://cdn.jsdelivr.net/gh/v2fly/geoip@release/geoip.dat"
    update_interval: "24h"        # 以 geoip.dat 的 mtime 计算下次更新时间
  api:
    enabled: true
    listen: ":3101"
    dashboard:
      url: "default"              # 或包含 dist 目录结构的 HTTP(S) ZIP
      update_interval: "24h"      # 仅远程 ZIP 使用
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
  sqlite_path: "queries.db"        # 相对于 data-dir
  host: "127.0.0.1:5432"
  user: "dns"
  password: "replace-this-password"
  db_name: "dns_db"

external_api:
  geo_ip_api_key: "none"
```

示例里的 `192.0.2.0/24`、`2001:db8::/32` 是文档地址，部署时要换成实际地址。只有在需要 Geo 分流时才配置国家分支；`JP` 这样的分支一旦命中，不会从 `default` 继承缺少的记录类型。

## `server`：监听与 GeoIP

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | 活动的非回环 IP 地址，端口 53 | IPv4/IPv6 UDP/TCP 地址列表；每个地址分别监听 UDP 和 TCP。生成时保存当前地址，网络地址变化后需手动更新。 |
| `geoip.enable_mmap` | `false` | `false` 使用 Go 堆内紧凑索引；`true` 使用文件映射的紧凑索引。重启后生效。 |
| `geoip.update_url` | jsDelivr 上的 v2fly GeoIP | GeoIP 数据文件的 HTTP(S) 直接下载地址；空值或未配置时使用默认上游。 |
| `geoip.update_interval` | `24h` | 更新间隔，使用 Go duration 写法，如 `12h`、`48h`；必须大于 0。 |

旧配置中的单个 `listen` 字符串需要改为列表；原 `listen_ipv6` 地址也应放入同一列表。仅在配置文件首次生成时检测本机地址，之后网络地址变化需手动更新。

Geo 查询顺序为：已有的 IPv4 `/24` 内存缓存 → `geo_cache.db` SQLite 缓存 → 本地 `geoip.dat` → API 兜底。`geoip.dat` 位于数据目录（软件包默认为 `/var/lib/calidns`）；文件不存在时继续使用缓存/API。

商业 API 代码位于 `external_api`，核心 `geo.Provider` 接口只接收 IP，并返回可空的 `CountryCode`、`SubLocation`、`ASN`、`ASNName`。内置 ip2location 实现默认由 `external_api.geo_ip_api_key` 配置；`none` 或空值关闭外部兜底。

`geoip.enable_mmap: true` 时，启动过程会在 `geoip.dat` 所在目录创建临时紧凑索引文件，映射成功后删除其目录项；因此该目录必须可写。关闭 mmap 时，运行期间只保留计算用的紧凑索引，不会把原始 `geoip.dat` 常驻内存。

下次更新时间 = **`geoip.dat` 的 mtime + `geoip.update_interval`**。启动时若文件不存在或已经到期，会立即尝试下载；未到期则等待剩余时间。下载内容必须是原始 `geoip.dat` 文件，不能是 ZIP、网页或 API JSON。服务先校验新文件并构建索引，成功后才替换磁盘文件和运行中的索引，无需重启；失败会保留旧文件/索引，并在 1 分钟至 1 小时后重试。成功更新会把文件 mtime 设为更新时间。由于 SQLite `/24` 缓存仍然优先，已有缓存条目在过期或删除前不会立即改用新数据。手工替换 `geoip.dat` 后仍需重启才能加载。

建议使用可信的 HTTPS 地址。自动更新需要数据目录可写；更新期间会短暂同时保留旧索引与新索引，内存占用可能高于平时。

## `server.api`：HTTP 管理接口

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 是否启动 HTTP API。 |
| `listen` | `:3101` | HTTP API 监听地址。 |
| `dashboard.url` | `default` | 空值关闭路由；`default` 提供内置 UI；HTTP(S) URL 下载包含 `index.html` 及 dist 资源的 ZIP。启用后统一在 `/dashboard` 提供。 |
| `dashboard.update_interval` | `24h` | 远程 Dashboard ZIP 的更新间隔；更新失败时保留上一个有效版本。 |
| `tokens` | 空 | Bearer Token 列表。**空列表表示不鉴权**，生产环境必须设置。 |
| `cors.allow_origins` | `["*"]` | 允许的来源；未设置时向所有来源开放。 |
| `cors.allow_methods` | `GET, POST, PUT, DELETE, OPTIONS, HEAD, PATCH` | 允许的 HTTP 方法。 |
| `cors.allow_headers` | `Content-Type, Authorization, X-Requested-With, Accept, Origin` | 允许的请求头。 |
| `cors.expose_headers` | 空 | 暴露给浏览器的响应头。 |
| `cors.max_age` | `0` | CORS 预检缓存秒数。 |
| `cors.allow_credentials` | `false` | 是否返回允许凭据的 CORS 响应头。 |

接口路径、请求与响应示例见 [API 文档](API.md)。`GET /api/server` 返回分组的 `geoip` 和 `dashboard` 配置；Dashboard URL 由请求的 host 和 scheme 生成。`PUT /api/server` 不会热切换这些设置，修改它们需编辑 YAML 并重启。

## `database` 与 `cluster`

`database` 存储 DNS 查询日志和 EDNS 记录。`type: sqlite` 时，`sqlite_path` 的相对路径以数据目录为基准，软件包默认得到 `/var/lib/calidns/queries.db`。

`type` 为其他值或省略时沿用 PostgreSQL。`geo_cache.db` 是独立的 SQLite Geo 缓存文件，也位于数据目录。

`cluster` 可省略。`mode: master` 时，主节点会把通过 API 进行的 Zone/部分服务器配置变更转发给 `slaves`；`mode: slave` 且设置 `master` 时，从节点启动时会从主节点的 `/api/config` 拉取 Zone 和业务配置。`listen`、数据库、集群地址及 GeoIP mmap/自动更新设置属于节点本地配置，不应假设会随主节点热同步。主从通信依赖各节点的 API 可访问以及相应 Token。

## `dns_data.db`：默认项与 Zone

首次启动会创建单行 `dns_defaults` 表，写入 TTL `300`、记录关闭、默认响应 `refuse`；Zone 保存在 `dns_zones` 表。两者均通过 Web API 管理，`config.yaml` 中的旧 Zone 和默认项不会导入。

每个 Zone 可以选择键的匹配方式。`mode: simple` 将键作为 DNS 域名进行不区分大小写的精确匹配，并允许末尾的点；`mode: golang` 将键直接作为 Go 正则表达式，例如 `'^www\.example\.com\.?$'`。省略 `mode` 时保留旧版自动判断行为：普通域名精确匹配，包含正则元字符的键按正则处理。请避免让多个 Zone 模式匹配同一域名：当前配置用 map 读取，重叠模式的优先顺序不保证固定。

每个 Zone 可设置：

| 字段 | 说明 |
| --- | --- |
| `mode` | `simple` 表示精确域名，`golang` 表示 Go 正则；省略时保留旧版自动判断。 |
| `default` | 国家代码未命中时使用的记录集，建议始终配置。 |
| `JP`、`US` 等 | 对应国家代码的完整记录集；**不会按记录类型与 `default` 合并**。 |
| `ttl` | 该 Zone 的 TTL 秒数；省略时继承数据库中的 `default_ttl`。 |
| `record` | 覆盖数据库中的 `default_record`。 |
| `fast_open` | 为 `true` 时始终选用 `default` 分支，不按 Geo 选择记录；如开启记录日志，Geo 仍可能用于日志。 |

未匹配 Zone 时使用数据库中的 `default_response`。匹配了 Zone、但该国家分支没有所查询的记录类型时，返回 NOERROR/NODATA，并附带程序生成的 SOA。Geo 查询失败或未命中时使用 `default` 分支。

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

域名目标建议使用以 `.` 结尾的完整域名。`other` 中不要写记录名、TTL 或 `IN`，这些由服务按查询域名和 Zone TTL 生成。API 新增 Zone 时会校验记录格式。当前 `cname` 仅在查询 CNAME 类型时返回；不会自动作为 A/AAAA 查询的别名应答。`ns` 提供 NS 记录应答，但不自动生成委派的 glue 记录。

需要管理记录时参见 [Zone API](API.md#6-新增--更新-zone)。
