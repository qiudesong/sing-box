# GoBGP 按需旁路

本模块通过客户端最终 DNS 响应学习目标，使用内嵌 GoBGP v4.10.0 向主路由通告 IPv4 `/32`、IPv6 `/128`。业务分流统一配置在 sing-box 原生 `route.rules` 和 `route.final`；旁路模块不实现代理协议，也不代替原生路由转发。

完整配置见 [examples/bgp-bypass.json](examples/bgp-bypass.json)，IPv4 部署、IPv6 扩展、RouterOS/Linux 配套配置和回滚见 [模块 README](../service/bgpbypass/README.md)。示例默认观察模式，节点及公网文档地址必须替换。

注册服务需要 `with_bgp`，可构建平台为 Linux、macOS、Windows、FreeBSD、OpenBSD、NetBSD 和 DragonFly BSD。模块不安装 Linux FIB、不设置 nftables 或系统策略路由；透明部署和网络验收使用 Linux TPROXY。平台可构建不等于透明转发已验收。

## 配置与原生选路

```json
{
  "route": {
    "rules": [
      {"inbound":["lan-tproxy","lan-tproxy6"],"port":53,"action":"hijack-dns"},
      {"inbound":["lan-tproxy","lan-tproxy6"],"bgp_bypass":"bypass-control","outbound":"proxy-default"},
      {"domain_suffix":["github.com"],"outbound":"proxy-default"}
    ],
    "final":"direct"
  }
}
```

这是接入片段，需已定义入口、代理出口及 `bypass-control` 服务。服务不配置独立的 `policies` 或 `final`，也不需要专用动态 outbound。DNS 服务器选择继续使用 `dns.servers/rules/final`。

1. DNS 最终响应返回前，服务用问题域名、答案 IP 和可确定的入口范围预判原生业务规则。
2. 有效代理结果进入域名/IP 租约；共享 IP 代理优先，代理之间按原生规则序号选择胜者。
3. 目标进入胜出逻辑出口的动态集合，并发布对应主机路由。
4. 流量到达 Linux 后，原生规则匹配 `bgp_bypass` 集合，由同一条规则的 `outbound` 执行原生转发或出口组选点。

动态集合直接查询分片租约状态，不复制完整关联，也不在逐包路径调用 BGP、磁盘或 HTTP。不保存客户端 IP。静态 `persistent_routes` 保留显式前缀/出口映射，用于不依赖 DNS 的应用，同样由原生集合接入规则选择出口。

### DNS 预判边界

复用原生规则对象的 Match，不复制域名/IP 匹配实现。支持域名、目标 IP、IP 版本、可评估规则集及其布尔组合，条件组合遵循原生语义，包括域名形式之间的 OR。`inbound` 条件必须对服务所有 `dns_inbounds` 有相同包含关系，不按客户端或入口分别存储决策。

端口、网络类型、进程、源地址、嗅探等未知条件导致整条规则被跳过，不能删除未知条件后继续匹配；逻辑规则同样整体处理。非终结动作、DNS 劫持、bypass、目标改写不在预判中执行。可评估的 reject 表示不产生代理学习结果。未命中使用原生默认出口。被跳过规则的 0 起始序号写入日志和 `/services/bgp-bypass/bypass-control/routes` 的 `dns_preflight_skipped_rules`。

可评估规则引用的规则集若含不支持条件，配置和更新会报错；动态集合规则自身始终不参加预判，防止反馈。新增上游条件默认不可预判，需检查后加入允许列表。规则集更新仍通过原生验证/回调触发有效租约重算。

### 集合接入与顺序

每个可学习代理出口和固定路由出口必须有对应的 `bgp_bypass` 路由规则。规则需要显式 route 出口，只能额外包含 inbound/network 条件，不支持取反或嵌套逻辑；必须覆盖服务全部学习入口及 TCP/UDP。缺失服务、出口、绑定、错误地址族、依赖环、FakeIP 或无限期 DNS 缓存均在配置检查时报错。

安全规则、拒绝和必要例外放在集合接入之前。BGP 表示引流到 Linux 继续决策，实际原生规则可以选择其他代理或直连；防环路独立表必须覆盖这些出站及返回 LAN 的路径。共享 IP 冲突语义约束集合归属，不覆盖前置安全/例外规则。

观察模式不建立会话、不通告、集合不匹配；其他原生规则正常工作。失去有效学习决策的新连接继续按原生规则和 final 处理，示例 final 为 direct。

## 生命周期、正确性与性能

DNS 最终响应观察覆盖同步、异步、缓存和预定义响应，不学习竞速败者。处理 A/AAAA、CNAME、HTTPS/SVCB 地址提示及答案明确引用的附加地址；不改写答案及签名。使用实际返回 TTL，别名链取相关最短 TTL，TTL=0 使用 1 秒，缓存命中使用剩余 TTL。成功学习的响应在等待提交和缓冲期间保留关联，放行前同步续期以覆盖返回后的完整 TTL；并发响应不会缩短已有租约，取消只释放待返回引用。域名/IP 关联独立过期。

活动引用通过原生 ConnectionTracker 获取最终逻辑出口，包含优先规则选择的其他代理；直连不增加代理引用。TCP/UDP 关闭只释放一次，包装保留 Reader/Writer 替换和 Upstream 能力。活动连接可延长通告，但不会恢复过期的集合成员供新连接使用。TUN 原生 flow 尚未提供同等保护，使用 TPROXY 部署。

固定前缀最长匹配，相同前缀不同出口报错；动态代理决策优先于固定映射，动态直连不覆盖固定映射。固定规则集必须是纯目标前缀并集。固定通告不受 TTL 清理，受排除范围和停机撤路控制。

控制器串行处理学习与更新，按期望/实际 RIB 对账恢复部分失败，批量增删。容量或队列满拒绝新增并计数，不驱逐有效路由；当前未实现 LRU、有界入队等待或 24 小时不活跃清理。首次提交最多等待 commit_timeout 和 settle_delay，超时仍返回 DNS；本地提交成功不是 RouterOS FIB 确认。

旁路服务不探测代理出口，也不因代理连接失败撤销通告。节点选择由原生出口组负责；代理全部不可用而 BGP 正常时，路由仍可指向旁路，连接可能失败。状态文件异步原子保存有效关联，恢复按当前原生规则重算，不恢复活动连接；损坏或不兼容时冷启动并告警。

GoBGP 只导出本模块路由、拒绝导入、不开放管理 gRPC，禁用 GR/LLGR。内置保留地址保护与显式排除共同生效，动态只发布主机前缀。正常关闭先撤路和关闭会话，再关闭透明入口；异常退出依靠邻居超时。

## 部署与 DNS 回切

[模块 README](../service/bgpbypass/README.md) 分别提供 IPv4 和 IPv6 的完整部署步骤：隔离传输网、独立直连表、双栈 TPROXY、FastTrack 排除、systemd 及 DNS 回切。详细诊断默认 loopback，独立健康端口限制来源。

`/livez` 仅表示运行。`/services/bgp-bypass/bypass-control/dns-readyz` 每 5 秒通过内部 DNS 路由无缓存查询根 NS，超时 3 秒；不能据此保证每个域名 DNS 上游、客户端入口和地址族都正常。`/services/bgp-bypass/bypass-control/readyz` 还检查配置、邻居和本地对账新鲜度，不表示代理出口可用。DNS 回切使用 `/services/bgp-bypass/bypass-control/dns-readyz`，同时验收实际双栈 DNS 和 NAT 状态。

## 观测与排障

```sh
curl http://127.0.0.1:9098/services/bgp-bypass/bypass-control/routes?limit=100
curl http://127.0.0.1:9098/services/bgp-bypass/bypass-control/routes/8.8.8.8
curl http://127.0.0.1:9098/metrics
```

分页使用返回的 `next` 作为 `after`（URL 编码），limit 为 1–500；它是当前快照的字典序分页，并非跨请求一致性事务。解释接口含域名、原生路由规则序号、租约有效期、按实际逻辑出口统计的活动引用、学习胜出出口、固定出口和本地通告状态。域名/IP 仅出现在诊断，不进入指标标签。健康端口只提供三个健康路径；详细接口非 loopback 监听必须设置 `secret`，请求携带 `Authorization: Bearer ...`，建议另用隧道/TLS 保护传输。

指标前缀 `sing_box_bgp_bypass_`：desired/actual_routes、dns_associations、pending_updates、conflicting_addresses、queue/capacity_rejected_total、commit_timeout_total、bgp_errors_total、reconciliations_total、announced/withdrawn_total、peer_up。`dns_wait_seconds_sum/count` 记录冷请求等待；`dns_observe_seconds` 直方图按 reason=cold/warm 记录完整观察钩子耗时。

```promql
histogram_quantile(0.99, sum by (le, service) (
  rate(sing_box_bgp_bypass_dns_observe_seconds_bucket{reason="warm"}[5m])
))
```

该指标不含 DNS 上游耗时。RIB 数量一致也不能证明远端 FIB/透明代理成功；需 RouterOS `/routing/route`、抓包和真实 TCP/UDP 请求交叉验证。BGP 日志通过 sing-box 日志接口输出，提交及状态文件错误均可定位服务标签。

## 上游合并与发布

`stable/testing` 保持纯镜像，custom 修改只在 main/feature。`upstream-pr.yml` 在镜像工作流成功后维护 `feature/upstream-stable -> main` PR，使用 merge，保留人工解决冲突的提交；遇冲突不覆盖已发布分支。初次 PR 可直接由 stable 起点创建，冲突由人工在 feature 分支解决。main 不强推，PR 需人工审核合并。

配置仅授权本仓库的 `UPSTREAM_PR_TOKEN`（Contents、Pull requests、Workflows 写权限），令牌身份需可创建 PR，并能触发后续 CI。未配置时工作流明确跳过；镜像同步原有令牌策略不变。不要把 token 写入配置或日志。GoBGP 升级单独提交，检查原生 API 与策略行为，不复制代理协议实现。custom tag、Draft Release、版本和镜像渠道沿用仓库现有约定。

上游升级逐项核对扩展点：

| 扩展点 | 升级检查 |
| --- | --- |
| `dns/router.go` + `dns/observer.go` | 所有最终响应路径恰好通知一次、缓存剩余 TTL、竞速败者不通知、观察发生在回调前 |
| `adapter/bgp_bypass.go` + `route/rule/rule_item_bgp_bypass.go` | 动态条件只匹配目标；原生 route 动作及 selector/urltest 每连接选择 |
| `service/bgpbypass/tracker.go` | tracker 调用时机、原生最终逻辑出口及入口元数据、关闭恰好释放一次及零拷贝替换能力 |
| `route/rule/rule_set.go` | 新增条件不能隐式成为预判通配条件；纯前缀提取语义；远程规则更新验证与回调 |
| `box.go` + 生命周期 scope | check 时标签完整、启动规则先可用、关闭顺序仍为撤路后透明入口 |
| `include/bgp*.go` | GoBGP 平台/tag gating、缺失标签错误、schema 注册 |
| CI/发布 | main/custom/mirror 约定、Linux 标签、跨平台检查和专用 PR token |

## 验证状态及发布门槛

已实现：配置/状态机、DNS/出口/BGP 集成、双栈部署模板/观测、Linux 网络测试脚本及上游 PR 自动维护。部署模板与验收脚本的存在不表示实机验证已完成。

本次原生路由集成的本地验证（macOS arm64，Go 1.27.0）：`with_bgp` 下 service/bgpbypass、route、route/rule、include 测试通过，service/bgpbypass、route、route/rule、dns 的 race 测试通过，无标签 include 测试通过。IPv4 与双栈示例均通过实际二进制的 `sing-box check`；macOS 检查副本仅清除了 Linux 专用 `route.default_mark`，部署文件保留该设置。配置检查仍有上游默认域名解析器的弃用提示。

Linux amd64/arm64 的 `with_bgp` 二进制已交叉构建通过。跨平台 CI 包含实际 include 注册入口的 macOS/Windows 配置、BGP 会话、状态保存和带标签构建检查。Windows/BSD 未做本次改动的实机运行验收，平台可构建也不表示透明转发已验收。

Linux CI `.github/workflows/bgp.yml` 运行契约、race、示例 check/schema、Linux arm64 编译、缺标签错误、DNS fuzz、微基准，以及四个 network namespace + FRR 集成。网络脚本只修改临时命名空间，保留日志。当前本地没有 Linux/FRR 运行环境，**网络脚本尚未实际执行**。脚本包含双栈 BGP/FIB、透明 TCP/UDP、TTL、活动 TCP 保留、1280 MTU 大响应、正常退出/状态恢复及崩溃撤路/直连回退；实际运行及完整 ICMP/PMTU、断链矩阵仍需在 Linux 完成。

**RouterOS CHR/实机验收未执行**。在目标版本检查双栈 FIB/next-hop、首包窗口、DNS 回切（包含旧 NAT 映射）、FastTrack 排除、非 TCP/UDP 防环路、ICMPv6/PMTU、断邻居、断电及恢复。FRR 成功不能替代此步骤。

**性能验收未执行**。在隔离 2 核/2GB Linux 上预热 1 万动态地址/10 万关联，以 1000 DNS QPS 运行 30 分钟，分别测关闭功能、启用稳态和冷学习压力。使用相同代理/网络/负载对照，采集 CPU、RSS、GC、队列拒绝和端到端 DNS/吞吐。门槛为关闭回退 ≤1%、启用稳定转发 ≤5%、暖观察钩子增量 P99 ≤2ms、模块 RSS 增量 ≤256MiB。单独的状态查询微基准不能代表原生规则预判和动态条件匹配的完整开销；当前全量期望/RIB 对账的冷学习开销也需在该压力下量化。

上线顺序：观察模式核对租约/共享 IP 冲突 → 接收过滤器限制测试 IP 并启用通告 → 完成 Linux/RouterOS/性能验收 → 扩大范围。回滚顺序：禁用通告并确认 RouterOS 撤路、回切双栈 DNS → 停服务/回退二进制 → 按需移除专用防火墙及策略路由。不要先退二进制而让路由仍指向已停止的旁路。

参考：[GoBGP](https://github.com/osrg/gobgp)、[原 OSPF 方案参考仓库](https://github.com/Einsfier/Xray-core)、[RouterOS connection](https://manual.mikrotik.com/docs/cli-reference/routing/bgp/connection/)、[RouterOS template](https://manual.mikrotik.com/docs/cli-reference/routing/bgp/template/)、[RouterOS packet flow](https://help.mikrotik.com/docs/spaces/ROS/pages/328227/Packet%2BFlow%2Bin%2BRouterOS)。
