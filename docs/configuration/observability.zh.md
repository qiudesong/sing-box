# 可观测性

`observability` 位于 sing-box 配置顶层，统一提供 Prometheus 指标、健康检查和服务诊断。

```json
{
  "observability": {
    "listen": "127.0.0.1",
    "listen_port": 9098,
    "metrics": true,
    "metrics_users": [],
    "diagnostics": false,
    "secret": "",
    "health_listen": "127.0.0.1",
    "health_port": 9099
  }
}
```

全部功能默认关闭。监听地址默认 `127.0.0.1`，端口默认 `0`（关闭）；启用指标或诊断必须设置非零 `listen_port`。

## 构建支持

Prometheus 使用官方 `github.com/prometheus/client_golang` SDK，需要 `with_prometheus` 构建标签。仓库默认构建列表包含该标签，但仍需通过配置开启指标。精简构建可移除标签；此时配置 `metrics: true` 会在配置检查时报错。健康检查和诊断不依赖该标签。

## 认证和监听

`secret` 对主端口的所有路径启用 `Authorization: Bearer <secret>` 认证。主端口监听非 loopback 地址时必填。远程抓取应使用可信管理网或 TLS 反向代理。

独立健康端口不要求认证，只提供健康路径，不提供指标和诊断。远端路由器需要探测时，将 `health_listen` 设置为可达的管理地址。

## HTTP 路径

全部接口只接受 GET。

| 路径 | 含义 |
| --- | --- |
| `/metrics` | 已启用且可用模块的指标；仅主端口，需要 `metrics` |
| `/livez` | Box 处于运行状态 |
| `/readyz` | Box 完成启动，且所有已注册模块就绪检查通过 |
| `/services/{type}/{tag}/livez` | 指定服务存活状态，服务需要提供对应检查 |
| `/services/{type}/{tag}/readyz` | 指定服务就绪状态，服务需要提供对应检查 |
| `/services/bgp-bypass/{tag}/dns-readyz` | BGP 服务已有 DNS 探测的缓存结果 |
| `/services/bgp-bypass/{tag}/routes` | BGP 分页路由诊断，需要 `diagnostics` |
| `/services/bgp-bypass/{tag}/routes/{ip}` | BGP 地址解释，需要 `diagnostics` |

服务类型与配置中的 `type` 一致，例如 `bgp-bypass`；将标签作为单个路径段进行 URL 编码。服务不存在、类型不匹配或接口不可用时返回 404。健康接口同时在主端口和健康端口提供，诊断仅在主端口提供。

没有模块检查时，`/readyz` 只取决于 Box 生命周期。配置 BGP 后，邻居断开或对账过期会使全局就绪返回 503；这不表示已主动检测代理出口连通性。DNS 主动探测仍由 BGP 模块执行，抓取指标不会发起探测。

旧 `services[].observability` 配置及根路径 `/routes`、`/dns-readyz` 已移除。将配置迁移至顶层，并更新探测 URL。

## Prometheus 抓取

```yaml
scrape_configs:
  - job_name: sing-box
    scrape_interval: 15s
    static_configs:
      - targets: ["127.0.0.1:9098"]
```

需要认证时增加 `authorization`，设置 `type: Bearer` 和指向令牌文件的 `credentials_file`。地址必须从 Prometheus 所在网络空间可达；配置中的 loopback 指 Prometheus 自身的网络空间。

## 指标及统计口径

| 指标族 | 内容 |
| --- | --- |
| `go_*`、`process_*` | SDK Go/runtime 及平台支持的进程指标 |
| `sing_box_traffic_bytes_total` | 应用字节，标签为 scope、kind、tag、network、direction |
| `sing_box_traffic_packets_total` | socket UDP 跟踪器观察到的数据报数 |
| `sing_box_flow_bytes_total`、`sing_box_flow_packets_total` | 原生 flow 的 IP 字节及报文观察数，包含内层 IP/传输层头 |
| `sing_box_flow_establishment_total`、`sing_box_flow_establishment_duration_seconds` | 原生 TCP flow 引擎报告的建立或提前关闭结果及耗时，不包含 UDP flow |
| `sing_box_connections` | 未关闭的跟踪连接及原生 flow，包括等待握手的连接 |
| `sing_box_connections_opened_total`、`sing_box_connections_closed_total` | 跟踪连接创建及关闭数 |
| `sing_box_connection_duration_seconds` | 从纳入跟踪到关闭的时长 |
| `sing_box_dial_total`、`sing_box_dial_duration_seconds` | 逻辑 socket 出口建立结果及耗时 |
| `sing_box_dns_requests_total`、`sing_box_dns_request_duration_seconds` | 分别统计 DNS 逻辑调用与上游尝试 |
| `sing_box_dns_cache_total` | 缓存判定：hit、stale、miss |
| `sing_box_bgp_bypass_*` | 原有路由、邻居、拒绝、对账和 DNS 观察指标 |
| `sing_box_outbound_group_selected` | 按出口组、节点及网络标识当前选择，值为 0/1 |
| `sing_box_outbound_delay_seconds`、`sing_box_outbound_delay_timestamp_seconds` | 最近 URLTest 结果和测量时间；抓取不触发测速 |
| `sing_box_ccm_*`、`sing_box_ocm_*` | 请求、消息、Token、费用；历史累计为 gauge，`_total` counter 记录当前 Box 生命周期的观测 |
| `sing_box_oom_*` | 内存策略和定时器/报告缓存状态 |
| `sing_box_power_*` | 已运行功耗 recorder 的采样结果及计数 |
| `sing_box_endpoint_*`、`sing_box_tailscale_*`、`sing_box_usbip_*` | 可用端点状态和缓存的状态汇总 |
| `tailscaled_inbound_bytes_total`、`tailscaled_outbound_bytes_total`、`tailscaled_inbound_packets_total`、`tailscaled_outbound_packets_total` | Tailscale 原生流量 Counter，保留上游名称、计数口径和 `path` 标签，增加 `endpoint` 标签 |

Tailscale 指标在订阅启动时立即采集，之后每两秒从本地后端刷新缓存，抓取不触发采集。流量直接来自各端点的上游指标 registry，不使用 peer 字节数求和，不再导出 `sing_box_tailscale_received_bytes`、`sing_box_tailscale_sent_bytes`。`sing_box_tailscale_peers_active` 沿用上游 `Active` 字段，目前表示最近约两分钟向该 peer 发送过包，与在线状态不同；该时间窗口由上游定义。

OOM 的 `trigger_bytes`、`armed_bytes` 按当前内存策略解释：内存限额和 Network Extension 策略在使用量达到上限阈值时触发，available 策略在剩余可用内存降至下限阈值时触发。available 阈值使用定时器最近一次有效采样，在获得有效采样前省略；抓取不会采样系统内存。

流量 `scope` 分为 `route`、`v2ray`、`ssm`，三者覆盖范围有重叠。各 scope 内 `kind="all"` 为汇总；`inbound`、`outbound`、`node`、`user` 是独立且重叠的统计视图，不能全部相加。`outbound` 表示逻辑路由出口，`node` 表示最终叶子节点。socket 应用字节不包含隧道和协议封装开销。原生 flow 回调观察完整内层 IP 报文，使用独立的 `flow_*` 指标族；计数发生在可选分片之前，也可能早于端口写入失败。比较应用负载速率时，应分别查看 socket 和原生 flow 字节。

V2Ray 的统计维度仍受自身配置约束。V2Ray 的 reset 和 SSM 的清零读取不影响 Prometheus 累计值。SSM 的 `all` 标签用冒号连接服务和入口标签。

`metrics_users` 是精确用户名白名单；只有其中的用户生成独立流量和用量序列。空列表只导出汇总，汇总始终包含全部用户。用量指标以 `user=""` 表示汇总，不把模型和不断增长的历史账期作为标签。请求数按成功解析的用量观察累计。

用量中的 `period_` gauge 只记录最近已知账期，`period_start_timestamp_seconds` 以数值表示账期起点，不使用动态账期标签。尚未观察到账期提示时不输出这些指标。

DNS 的 `kind="exchange"`、`kind="lookup"` 记录完成的逻辑调用；`upstream` 记录实际传输尝试，包括竞速请求；`refresh` 记录后台刷新。缓存命中不增加上游请求数。result 为数字响应码或 error/timeout/canceled；query_type 为数字类型，逻辑 Lookup 使用 0。缓存判定可能包含合并请求的重新检查，不能视作客户端请求数。

新增耗时直方图的秒单位桶为 `0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, +Inf`；原有 BGP 桶保持不变。

Box 重建时 counter 可以重置。未启用模块或平台不支持的指标省略。抓取不会启动 profiling、功耗记录、远程状态查询或测速。订阅型指标在首个状态快照到达前不输出。域名、地址、连接 ID 和原始错误文本不进入指标标签。

```promql
# 路由上传速率，避免跨统计维度重复相加。
sum(rate(sing_box_traffic_bytes_total{scope="route",kind="all",direction="upload"}[5m]))

# DNS 逻辑 Exchange 的 P99。
histogram_quantile(0.99, sum by (le) (
  rate(sing_box_dns_request_duration_seconds_bucket{kind="exchange"}[5m])
))
```
