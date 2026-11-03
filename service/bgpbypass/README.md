# GoBGP 按需旁路：配置与 RouterOS + Linux 部署

本模块从客户端最终 DNS 响应建立域名/IP 租约，按原生路由规则预判代理出口，通过内嵌 GoBGP v4.10.0 向主路由发布 IPv4 `/32`、IPv6 `/128`。普通直连流量留在主路由；命中通告的 TCP/UDP 流量进入 Linux TPROXY，再由原生 `route.rules` 选择出口。

本文给出一个明确拓扑下的完整旁路配置，包括 sing-box、Linux 网络/防火墙/systemd、RouterOS BGP/防环路/DNS 回切/FastTrack，以及验证和回滚。已有 WAN 接入、LAN bridge、DHCP 和基础防火墙继续由主路由维护，不使用清空配置的方式部署。

**验证边界：RouterOS CHR/实机、Linux 网络部署及持续性能验收尚未完成。** RouterOS 命令按 **7.26 CLI 文档**编写。部署前核对目标设备的 BGP instance、IPv6 NAT、IPv6 FastTrack 和 Fetch 参数。

## 1. 工作方式与当前能力

```text
LAN 客户端 -- DNS:53 --> RouterOS LAN 地址 -- DNAT --> Linux TPROXY
                                                        |
                                      最终 DNS 响应 → 原生规则预判 → 租约
                                                        |
RouterOS main FIB <-------------- eBGP 目标主机路由 -------+
       |
       +-- 普通目标 --> WAN
       +-- 代理目标 --> Linux TPROXY --> 现有代理出口

Linux 发回 RouterOS 的非本机流量 --> bypass-direct 表 --> LAN 或 WAN
```

- DNS 上游由 `dns.servers/rules/final` 选择；业务分流统一使用 `route.rules` 和 `route.final`。服务预判复用可用的原生规则，`bgp_bypass` 匹配条件将学习结果接回原生路由。
- 全网共享目标 IP 决策，不记录客户端。共享 IP 代理优先，代理之间按原生路由规则序号选择。已有连接保留原出口。
- 租约使用实际响应 TTL；缓存命中使用剩余 TTL，别名链取相关最短 TTL，TTL=0 使用 1 秒。成功学习的响应在提交和缓冲等待期间保留关联，放行前同步续期，覆盖返回后的完整 TTL；并发响应不会缩短已有租约。租约失效且活动连接全部释放后，再等待 `withdraw_delay` 撤路。
- `advertise: false` 是观察模式：不建立 BGP 会话，不通告路由，`bgp_bypass` 条件始终不匹配，连接继续按原生路由处理；仍学习和展示状态。
- 固定路由不受 TTL 清理，受排除范围和停机撤路控制；动态有效代理决策优先于固定出口。
- 容量满拒绝新增；队列满丢弃本次学习事件，但仍返回 DNS 答案，后续访问可能直连。**LRU、重复事件合并、有界入队等待、24h 不活跃清理或 TTL 上限目前均未实现，也没有对应配置字段。**
- 不使用 FakeIP，不依赖 sniffing/reverse_mapping；与 FakeIP 或 `dns.disable_expire` 组合会拒绝启动。
- GoBGP 不安装 Linux FIB，不开放管理 gRPC，只输出本模块路由并拒绝导入。Linux 透明转发和系统路由由本文部署配置提供。
- 活动连接保护使用 TCP/UDP 连接跟踪路径；TUN 原生 flow 路径尚未实现同等引用管理，本文使用 TPROXY。

更完整的实现、上游维护和验收说明见 [设计文档](../../docs/bgp-bypass.md)。

## 2. 共用准备与阅读顺序

IPv4 和 IPv6 设置分别放在第 3、4 节。**仅部署 IPv4：完成第 2、3、5 节；部署双栈：再完成第 4 节。** 第 4 节是在 IPv4 部署上增加 IPv6，不需要重复创建 sing-box 服务、RouterOS 路由表或 BGP instance。

两部分都按“配置文件 → RouterOS → Linux → DNS → 验证”组织。准备阶段保持 `advertise: false`、DNS DNAT 禁用；全部文件准备好后按第 5 节启动，再执行各地址族的验证。

| 共用项目 | 示例 | 部署要求 |
| --- | --- | --- |
| RouterOS LAN / WAN | `bridge-lan` / `ether1` | 已有可用的家庭网络及基础防火墙 |
| RouterOS 传输口 | `ether2` | 不加入 LAN bridge，不包含其他客户端 |
| Linux 传输口 | `bypass0` | 替换成实际网卡名，如 `enp1s0` |
| ASN | RouterOS `65001`，Linux `65002` | 独立私有 ASN |
| Linux 表 / 优先级 | `100` / `100` | 各地址族分别配置，不与现有规则冲突 |
| Linux mark | 出站 `1`，TPROXY `2`，掩码 `0xff` | 保留低 8 位，避免和其他软件冲突 |
| RouterOS 路由表 | `bypass-direct` | 两个地址族共用名称，不导入旁路 BGP 路由 |
| RouterOS 连接标记 | `bgp-bypass` | 仅排除 FastTrack，不用于双向路由标记 |

命令是首次部署模板，不能反复执行 `add` 产生重复项。执行前导出 RouterOS 配置、使用 Safe Mode，并核对规则顺序。WAN、LAN、DHCP 和基础防火墙应已可用；本文不是空白路由器的一键初始化脚本。

Linux 示例为独立 Debian 12/13 主机或 VM，使用 systemd-networkd。已有 NetworkManager、ifupdown、UFW 等管理器时，将等价设置合并到现有方式，不能让多套管理器同时管理同一接口。

构建需启用 `with_bgp` 及所选代理协议需要的其他标签。Linux custom 构建已默认包含 BGP 标签；用 `sing-box version` 确认。以下在仓库根目录执行：

```sh
go build -tags with_bgp -o sing-box ./cmd/sing-box
./sing-box check -c docs/examples/bgp-bypass.json
```

Linux 安装工具并创建配置目录：

```sh
sudo apt-get update
sudo apt-get install -y nftables iproute2 curl dnsutils ca-certificates
sudo mkdir -p /etc/nftables.d /etc/sing-box
```

本文按地址族分别使用 `bgp-bypass4/6.nft`、`bgp-bypass-network4/6.service`。

## 3. IPv4 完整部署

### 3.1 IPv4 地址与前置条件

| 项目 | 示例 |
| --- | --- |
| RouterOS LAN | `192.168.88.1/24` |
| RouterOS / Linux 传输地址 | `192.168.66.1/24` / `192.168.66.2/24` |
| WAN 下一跳 | `198.51.100.1`，必须替换为真实网关 |
| 代理服务器 | `192.0.2.10:1080`、`192.0.2.20:1080`，必须替换 |
| TPROXY | `lan-tproxy`，TCP/UDP `12345` |
| BGP next-hop | `192.168.66.2` |

`192.0.2.0/24`、`198.51.100.0/24` 是文档网段，不能用于真实公网通信。主路由 main 表必须能解析真实 WAN 下一跳。本文采用静态网关；PPPoE、DHCP WAN 或多 WAN 需将独立表的默认路由同步纳入现有更新逻辑。

### 3.2 sing-box IPv4 完整配置

以下是从 [双栈契约示例](../../docs/examples/bgp-bypass.json) 提取的 IPv4 配置，默认观察模式。保存到 `/etc/sing-box/config.json`，替换代理地址，并将实际管理网段和代理服务器公网 IP 加入 `exclude_prefixes`。仅启用 IPv4 BGP 不会自动禁止 DNS 返回 AAAA；IPv6 目标不会因此产生可用的 IPv6 旁路通告。

```json
{
  "log": {
    "level": "info"
  },
  "dns": {
    "servers": [
      {
        "type": "udp",
        "tag": "dns-direct",
        "server": "223.5.5.5",
        "detour": "direct"
      },
      {
        "type": "https",
        "tag": "dns-proxy",
        "server": "1.1.1.1",
        "detour": "proxy-default"
      }
    ],
    "rules": [
      {
        "domain_suffix": [
          "google.com",
          "openai.com",
          "github.com",
          "githubusercontent.com"
        ],
        "server": "dns-proxy"
      }
    ],
    "final": "dns-direct"
  },
  "inbounds": [
    {
      "type": "tproxy",
      "tag": "lan-tproxy",
      "listen": "0.0.0.0",
      "listen_port": 12345
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    },
    {
      "type": "socks",
      "tag": "proxy-default",
      "server": "192.0.2.10",
      "server_port": 1080
    },
    {
      "type": "socks",
      "tag": "proxy-jp",
      "server": "192.0.2.20",
      "server_port": 1080
    }
  ],
  "route": {
    "default_mark": 1,
    "rules": [
      {
        "inbound": [
          "lan-tproxy"
        ],
        "port": 53,
        "action": "hijack-dns"
      },
      {
        "inbound": [
          "lan-tproxy"
        ],
        "bgp_bypass": "bypass-control",
        "action": "route",
        "outbound": "proxy-jp"
      },
      {
        "inbound": [
          "lan-tproxy"
        ],
        "bgp_bypass": "bypass-control",
        "action": "route",
        "outbound": "proxy-default"
      },
      {
        "domain_suffix": [
          "example.cn"
        ],
        "outbound": "direct",
        "action": "route"
      },
      {
        "domain_suffix": [
          "google.com",
          "openai.com"
        ],
        "outbound": "proxy-jp",
        "action": "route"
      },
      {
        "rule_set": [
          "proxy-domains"
        ],
        "outbound": "proxy-default",
        "action": "route"
      }
    ],
    "final": "direct",
    "rule_set": [
      {
        "type": "inline",
        "tag": "proxy-domains",
        "rules": [
          {
            "domain_suffix": [
              "github.com",
              "githubusercontent.com"
            ]
          }
        ]
      }
    ]
  },
  "services": [
    {
      "type": "bgp-bypass",
      "tag": "bypass-control",
      "advertise": false,
      "dns_inbounds": [
        "lan-tproxy"
      ],
      "state_path": "/var/lib/sing-box/bgp-bypass-state.json",
      "bgp": {
        "local_asn": 65002,
        "router_id": "192.168.66.2",
        "listen_port": -1,
        "next_hop_ipv4": "192.168.66.2",
        "keepalive": "3s",
        "hold_time": "9s",
        "peers": [
          {
            "tag": "routeros-v4",
            "address": "192.168.66.1",
            "local_address": "192.168.66.2",
            "asn": 65001,
            "families": [
              "ipv4-unicast"
            ]
          }
        ]
      },
      "persistent_routes": [],
      "exclude_prefixes": [
        "192.168.66.0/24"
      ],
      "limits": {
        "max_dynamic_routes": 10000,
        "max_dns_associations": 100000,
        "max_pending_updates": 4096
      },
      "timing": {
        "commit_timeout": "250ms",
        "settle_delay": "100ms",
        "withdraw_delay": "60s"
      },
      "observability": {
        "listen": "127.0.0.1",
        "listen_port": 9098,
        "metrics": true,
        "diagnostics": true,
        "health_listen": "192.168.66.2",
        "health_port": 9099
      }
    }
  ]
}
```

这个例子仅代理列出的域名，不是完整的地区分流规则库。YouTube 视频还会使用 `googlevideo.com` 等域名；按需求在 DNS 规则和原生业务路由规则中补充，或让两者引用同一份域名规则集。使用域名作为代理服务器地址时，单独配置直接可达的启动解析路径，并排除节点 IP，避免引流节点连接形成自循环。

可选固定 IPv4 路由：将下面数组内容加入服务的 `persistent_routes`。这是配置片段，不是完整配置：

```json
{
  "persistent_routes": [
    {
      "ip_cidr": [
        "1.1.1.1/32"
      ],
      "outbound": "proxy-default"
    }
  ]
}
```

#### 原生路由与动态集合的衔接

`bgp_bypass: "bypass-control"` 是原生路由的匹配条件。服务判断目标 IP 的有效胜出出口是否等于该条规则的 `outbound`，真正转发由原生路由和 outbound 执行。

每个预判可能选择的代理出口及固定路由出口，都需要一条对应的 `bgp_bypass` 接入规则。该条件只允许与 `inbound`、`network` 组合，要求显式 `route` 出口，不能嵌入逻辑规则或取反。接入规则必须覆盖服务的全部学习入口及 TCP/UDP；端口等特殊例外单独放在接入规则之前。

示例顺序为 DNS 劫持 → 动态集合接入 → 原生域名/规则集业务规则 → `route.final`。已有拒绝、安全规则和必要例外放在接入规则之前。原生规则仍可覆盖学习出口，因此 BGP 通告代表“引流到 Linux 进一步处理”，不保证每条连接都代理；独立直连表仍然必需。

共享 IP 的代理优先只决定动态集合中的胜出结果。安全规则、嗅探获得的域名规则或其他前置原生规则仍按原生顺序执行。失去有效租约的目标不再命中动态集合，后续连接按原生规则和 `route.final` 处理；示例使用 `direct` 兜底。

固定前缀保留显式出口映射，用于不经过 DNS 的目标；同样通过对应的原生 `bgp_bypass` 规则选择出口，仍受前置原生规则约束。

### 3.3 RouterOS IPv4 地址、防环路与 BGP

确认 `ether2` 未加入 LAN bridge。创建共用路由表一次，然后配置 IPv4：

```routeros
/ip address add address=192.168.66.1/24 interface=ether2 comment="bgp-bypass transit"
/ip settings set rp-filter=loose send-redirects=no
/routing table add name=bypass-direct fib
/ip route add dst-address=0.0.0.0/0 gateway=198.51.100.1@main routing-table=bypass-direct comment="bgp-bypass WAN4"
/ip route add dst-address=192.168.88.0/24 gateway=bridge-lan routing-table=bypass-direct comment="bgp-bypass LAN4"
/ip route add dst-address=192.168.66.0/24 gateway=ether2 routing-table=bypass-direct comment="bgp-bypass transit4"
/ip route add blackhole dst-address=0.0.0.0/0 distance=254 routing-table=bypass-direct comment="bgp-bypass fail closed table4"
/ip firewall mangle add chain=prerouting in-interface=ether2 dst-address-type=!local action=mark-routing new-routing-mark=bypass-direct passthrough=no comment="bgp-bypass route4"
/ip firewall mangle move [find where comment="bgp-bypass route4"] 0
```

`@main` 只解析真实 WAN 下一跳，不表示将目的地址查找回退到 main。高距离 blackhole 默认路由避免 WAN 路由失效后回落到含旁路通告的 main。独立表必须包含 LAN 路由，否则 Linux 返回客户端的包会被送往 WAN；多 LAN/VLAN、VPN 需同样补齐。

按 `in-interface=ether2` 设置路由标记，覆盖 Linux 出站、直连及转发，不按连接双向设置。目的为 RouterOS 自身的控制流量仍由本机处理。

已有 WAN masquerade 若未覆盖 Linux 传输网段，再添加：

```routeros
/ip firewall nat add chain=srcnat src-address=192.168.66.0/24 out-interface=ether1 action=masquerade comment="bgp-bypass Linux WAN4"
```

先创建接收过滤器、拒绝导出过滤器及共用 BGP instance，再创建 IPv4 邻居。测试阶段应把 accept 收窄到测试目标 IP：

```routeros
/routing filter rule
add chain=bypass-export rule="reject" comment="bgp-bypass export deny"
add chain=bypass-v4 rule="if (dst in 0.0.0.0/8 || dst in 10.0.0.0/8 || dst in 100.64.0.0/10 || dst in 127.0.0.0/8 || dst in 169.254.0.0/16 || dst in 172.16.0.0/12 || dst in 192.0.0.0/24 || dst in 192.0.2.0/24 || dst in 192.88.99.0/24 || dst in 192.168.0.0/16 || dst in 198.18.0.0/15 || dst in 198.51.100.0/24 || dst in 203.0.113.0/24 || dst in 224.0.0.0/3) { reject }" comment="bgp-bypass reserved4"
add chain=bypass-v4 rule="if (dst in 0.0.0.0/0 && dst-len == 32) { accept }; reject" comment="bgp-bypass hosts4"

/routing bgp instance add name=bypass as=65001 router-id=192.168.66.1 routing-table=main
/routing bgp connection
add name=bypass-v4 instance=bypass local.role=ebgp local.address=192.168.66.1 remote.address=192.168.66.2 remote.as=65002 afi=ip connect=no listen=yes hold-time=9s keepalive-time=3s input.filter=bypass-v4 output.filter-chain=bypass-export use-bfd=no
```

代理服务器的公网 IP 和额外管理网段也应在 accept 前拒绝。Linux 主动连接，RouterOS 监听；不配置默认路由起源或重分发。如果还有其他 BGP/OSPF 邻居，禁止将旁路路由再分发出去，不能只依赖 NO_EXPORT。

GoBGP 已关闭 GR；核对 RouterOS 协商结果，确保 GR/LLGR 不会保留故障旁路路由。不要添加未经目标版本确认的 GR 参数。本方案不依赖 BFD。

### 3.4 Linux IPv4 地址与内核设置

保存 `/etc/systemd/network/30-bgp-bypass.network`：

```ini
[Match]
Name=bypass0

[Network]
Address=192.168.66.2/24
IPv6AcceptRA=no
LinkLocalAddressing=no

[Route]
Destination=0.0.0.0/0
Gateway=192.168.66.1
```

该 IPv4 基础文件不接受 RA、不自动配置链路本地地址；启用 IPv6 时由第 4 节的 drop-in 补充。保存 `/etc/sysctl.d/90-bgp-bypass4.conf`：

```ini
net.ipv4.ip_forward=1
net.ipv4.conf.all.rp_filter=0
net.ipv4.conf.default.rp_filter=0
net.ipv4.conf.bypass0.rp_filter=0
net.ipv4.conf.all.send_redirects=0
net.ipv4.conf.default.send_redirects=0
net.ipv4.conf.bypass0.send_redirects=0
net.ipv4.conf.all.accept_redirects=0
```

在本机控制台或有备用管理路径时应用；重新配置默认路由可能影响 SSH：

```sh
sudo systemctl enable --now systemd-networkd
sudo networkctl reload
sudo networkctl reconfigure bypass0
sudo sysctl --system
ip -4 -br address show dev bypass0
ip route show default
```

### 3.5 Linux IPv4 nftables 与策略路由

保存 `/etc/nftables.d/bgp-bypass4.nft`：

```nft
table ip bgp_bypass4 {
  chain prerouting {
    type filter hook prerouting priority mangle; policy accept;
    iifname != "bypass0" return
    meta mark & 0xff == 1 return
    ct direction reply return

    # 先接收 DNAT 到本机的 DNS；必须经过 TPROXY 才有 dns_inbounds 元数据。
    ip daddr 192.168.66.2 meta l4proto { tcp, udp } th dport 53 tproxy to :12345 meta mark set (meta mark & 0xffffff00) | 2 accept

    # 本机 SSH/健康/BGP 控制流量不劫持；不要全局放过目标端口 22/179 等。
    fib daddr type local return
    meta l4proto { tcp, udp } tproxy to :12345 meta mark set (meta mark & 0xffffff00) | 2 accept
  }

  chain input {
    type filter hook input priority filter; policy drop;
    iifname "lo" accept
    ct state invalid drop
    ct state established,related accept
    ip protocol icmp accept
    iifname "bypass0" meta mark & 0xff == 2 accept
    iifname "bypass0" ip saddr { 192.168.66.1, 192.168.88.0/24 } tcp dport 22 accept
    iifname "bypass0" ip saddr 192.168.66.1 tcp dport 9099 accept
  }

  chain forward {
    type filter hook forward priority filter; policy drop;
    ct state invalid drop
    ct state established,related accept
    # 非 TCP/UDP 报文可回送 RouterOS 独立表，包含 ICMP 和 PMTU。
    iifname "bypass0" oifname "bypass0" ip saddr 192.168.88.0/24 accept
  }
}
```

该表只处理 IPv4，不提供 IPv6 防火墙策略；未部署第 4 节时，IPv6 安全策略仍由主机现有配置负责。不要 `flush ruleset`。input/forward 是专用主机基础过滤，其他业务需合并必要规则；其他 base chain 的 drop 不会被本表 accept 覆盖。不要全局放过目标端口 22/179，否则对应代理流量也会绕过 TPROXY。无需 OUTPUT TPROXY 或 Linux masquerade。

保存 `/etc/systemd/system/bgp-bypass-network4.service`：

```ini
[Unit]
Description=IPv4 network rules for sing-box BGP bypass
Wants=network-online.target
After=network-online.target nftables.service
Before=sing-box-bypass.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/usr/sbin/ip rule del priority 100 fwmark 2/0xff lookup 100
ExecStartPre=-/usr/sbin/nft delete table ip bgp_bypass4
ExecStart=/usr/sbin/ip route replace local 0.0.0.0/0 dev lo table 100
ExecStart=/usr/sbin/ip rule add priority 100 fwmark 2/0xff lookup 100
ExecStart=/usr/sbin/nft -f /etc/nftables.d/bgp-bypass4.nft
ExecStop=-/usr/sbin/nft delete table ip bgp_bypass4
ExecStop=-/usr/sbin/ip rule del priority 100 fwmark 2/0xff lookup 100
ExecStop=-/usr/sbin/ip route del local 0.0.0.0/0 dev lo table 100

[Install]
WantedBy=multi-user.target
```

优先级 100、表 100 及 `ip bgp_bypass4` 必须专用。用 `command -v ip nft` 核对路径。由本单元加载 nft 文件，不再通过 `/etc/nftables.conf` 重复加载。通告期间不要单独重启网络单元。

### 3.6 RouterOS IPv4 防火墙与 FastTrack

规则放在相关 drop 前，保留必须优先执行的安全规则。`add` 默认追加，启用 DNS DNAT 前需检查并调整位置。不要将传输口整体加入可信 LAN：

```routeros
/ip firewall filter
add chain=input in-interface=ether2 src-address=192.168.66.2 dst-address=192.168.66.1 protocol=tcp dst-port=179 action=accept comment="bgp-bypass BGP4"
add chain=input in-interface=bridge-lan dst-address=192.168.88.1 protocol=udp dst-port=53 action=accept comment="bgp-bypass fallback DNS4 UDP"
add chain=input in-interface=bridge-lan dst-address=192.168.88.1 protocol=tcp dst-port=53 action=accept comment="bgp-bypass fallback DNS4 TCP"
add chain=forward in-interface=bridge-lan out-interface=ether2 src-address=192.168.88.0/24 action=accept comment="bgp-bypass LAN to Linux4"
add chain=forward in-interface=ether2 out-interface=bridge-lan dst-address=192.168.88.0/24 action=accept comment="bgp-bypass Linux to LAN4"
add chain=forward in-interface=ether2 out-interface=ether1 src-address=192.168.66.2 action=accept comment="bgp-bypass Linux WAN4"
add chain=forward in-interface=ether2 out-interface=ether1 src-address=192.168.88.0/24 protocol=icmp action=accept comment="bgp-bypass forwarded ICMP4"
```

基础防火墙应已有 established/related 放行、invalid 丢弃、必要 ICMP 和 WAN 入站保护。若限制 OUTPUT，额外允许到 Linux TCP 9099 和备用 DNS 查询。其他非 TCP/UDP 协议按需放行，不会被 sing-box 代理。

在 forward/mangle 双向标记经过旁路接口的连接，排除 FastTrack，避免绕过独立表：

```routeros
/ip firewall mangle
add chain=forward in-interface=ether2 connection-mark=no-mark action=mark-connection new-connection-mark=bgp-bypass passthrough=yes comment="bgp-bypass no fasttrack in4"
add chain=forward out-interface=ether2 connection-mark=no-mark action=mark-connection new-connection-mark=bgp-bypass passthrough=yes comment="bgp-bypass no fasttrack out4"

/ip firewall filter set [find where action=fasttrack-connection] connection-mark=no-mark
```

标记规则应在会提前结束 forward/mangle 的规则之前。已有 FastTrack 若匹配其他标记，需合并排除条件，不能直接覆盖。已经 FastTrack 的连接不会因新通告立即改路，验收使用新连接，必要时只清理测试 conntrack。

### 3.7 RouterOS IPv4 DNS 与回切动作

客户端通过 DHCP 使用 RouterOS LAN DNS 地址。备用解析必须直接可达；已有 DoH、forwarder 或动态 DNS 设置时核对实际路径：

```routeros
/ip dns set allow-remote-requests=yes servers=223.5.5.5,119.29.29.29
/ip dhcp-server network set [find where address="192.168.88.0/24"] dns-server=192.168.88.1

/ip firewall nat
add chain=dstnat in-interface=bridge-lan dst-address=192.168.88.1 protocol=udp dst-port=53 action=dst-nat to-addresses=192.168.66.2 to-ports=53 disabled=yes comment="bgp-bypass-dns"
add chain=dstnat in-interface=bridge-lan dst-address=192.168.88.1 protocol=tcp dst-port=53 action=dst-nat to-addresses=192.168.66.2 to-ports=53 disabled=yes comment="bgp-bypass-dns"
```

仅重定向 LAN 发往网关的 TCP/UDP 53，不重定向 Linux 请求或应用 DoH/DoT。DNAT 规则在已有同目标 redirect/DNAT 前，初始保持禁用。

注册 IPv4 的启用/停用动作，供第 5 节共用健康脚本调用：

```routeros
/system script add name=bgp-bypass-dns4-enable comment="bgp-bypass-dns-enable" policy=read,write,test source={
    /ip firewall nat enable [find where comment="bgp-bypass-dns"];
}
/system script add name=bgp-bypass-dns4-disable comment="bgp-bypass-dns-disable" policy=read,write,test source={
    /ip firewall nat disable [find where comment="bgp-bypass-dns"];
}
```

### 3.8 IPv4 验证与撤销

完成第 5 节启动后，在 Linux 检查；测试客户端地址按实际替换：

```sh
ping -c 3 192.168.66.1
ip route get 1.1.1.1
ip route get 192.168.88.100
ip rule show
ip route show table 100
sudo nft list table ip bgp_bypass4
```

从 LAN 客户端测试 DNS；传输使用 IPv4 仍可查询 A 和 AAAA：

```sh
dig @192.168.66.2 github.com A
dig @192.168.66.2 github.com AAAA
dig +tcp @192.168.66.2 github.com A
```

测试通告后检查 `bypass-v4` 会话、目标 `/32` 和下一跳 `192.168.66.2`，并验证新的 TCP/UDP、ICMP 和 PMTU：

```routeros
/routing bgp session print detail
/ip route print detail where bgp
/ip route print detail where routing-table=bypass-direct
/ip firewall mangle print stats
/ip firewall nat print stats where comment="bgp-bypass-dns"
```

撤销 IPv4 时，先按第 7 节停用共用调度器，再执行：

```routeros
/ip firewall nat disable [find where comment="bgp-bypass-dns"]
/routing bgp connection disable [find where name="bypass-v4"]
```

## 4. IPv6 扩展部署

本节在第 3 节基础上增加 IPv6，复用同一个 sing-box 进程、策略、状态、RouterOS `bypass-direct` 表及 BGP instance。保留 IPv4 配置；以下片段均为追加，不覆盖已有数组或文件。

### 4.1 IPv6 地址与前置条件

| 项目 | 示例 |
| --- | --- |
| RouterOS LAN | `2001:db8:100:88::1/64` |
| RouterOS / Linux 传输 ULA | `fd66::1/64` / `fd66::2/64` |
| RouterOS / Linux 传输公网地址 | `2001:db8:100:66::1/64` / `2001:db8:100:66::2/64` |
| WAN 下一跳 | `2001:db8:ffff::1` |
| TPROXY | `lan-tproxy6`，TCP/UDP `12346` |
| BGP next-hop | `fd66::2` |

所有 `2001:db8::/32` 地址都必须换成真实地址。主路由和 LAN 应已能原生 IPv6 上网；Linux 需另一个实际委派的 /64 及回程路由。ULA 只用于 BGP/传输，不能作为普通公网连接源地址。没有公网地址和回程时，先仅部署 IPv4；本文不使用 NAT66 代替原生路由。

动态 PD 时，应同步更新 LAN、传输地址、独立表、排除前缀及防火墙。WAN 链路本地网关需带接口作用域，按目标 RouterOS CLI 确认写法。

### 4.2 sing-box IPv6 追加配置

向 `inbounds` 数组追加：

```json
{
  "type": "tproxy",
  "tag": "lan-tproxy6",
  "listen": "::",
  "listen_port": 12346
}
```

向 `bypass-control.bgp` 对象增加 `next_hop_ipv6`，并将下面的邻居对象追加到原 `peers` 数组：

```json
{
  "next_hop_ipv6": "fd66::2",
  "peers": [
    {
      "tag": "routeros-v6",
      "address": "fd66::1",
      "local_address": "fd66::2",
      "asn": 65001,
      "families": [
        "ipv6-unicast"
      ]
    }
  ]
}
```

向服务 `dns_inbounds` 和第 3 节 DNS 劫持、两条 `bgp_bypass` 路由规则的 `inbound` 数组分别追加 `"lan-tproxy6"`。向 `exclude_prefixes` 追加 `"fd66::/64"`、实际 LAN 公网 IPv6 前缀、传输公网 IPv6 前缀和代理服务器公网 IPv6。不要创建第二个旁路服务。

需要固定 IPv6 路由时，向原 `persistent_routes` 数组追加：

```json
{
  "ip_cidr": [
    "2606:4700:4700::1111/128"
  ],
  "outbound": "proxy-default"
}
```

按上述追加后，双栈结构可对照 [完整契约示例](../../docs/examples/bgp-bypass.json)。BGP Router ID 仍为原 IPv4 格式标识，无需为 IPv6 另改。

### 4.3 RouterOS IPv6 地址、防环路与 BGP

复用已创建的 `bypass-direct` 表，增加 IPv6 路由，不能遗漏返回 LAN 的前缀：

```routeros
/ipv6 address add address=fd66::1/64 interface=ether2 advertise=no comment="bgp-bypass transit"
/ipv6 address add address=2001:db8:100:66::1/64 interface=ether2 advertise=no comment="bgp-bypass transit global"

/ipv6 route add dst-address=::/0 gateway=2001:db8:ffff::1@main routing-table=bypass-direct comment="bgp-bypass WAN6"
/ipv6 route add dst-address=2001:db8:100:88::/64 gateway=bridge-lan routing-table=bypass-direct comment="bgp-bypass LAN6"
/ipv6 route add dst-address=fd66::/64 gateway=ether2 routing-table=bypass-direct comment="bgp-bypass transit6"
/ipv6 route add dst-address=2001:db8:100:66::/64 gateway=ether2 routing-table=bypass-direct comment="bgp-bypass transit global6"
/ipv6 route add blackhole dst-address=::/0 distance=254 routing-table=bypass-direct comment="bgp-bypass fail closed table6"
/ipv6 firewall mangle add chain=prerouting in-interface=ether2 dst-address-type=!local dst-address=!ff00::/8 action=mark-routing new-routing-mark=bypass-direct passthrough=no comment="bgp-bypass route6"
/ipv6 firewall mangle move [find where comment="bgp-bypass route6"] 0
```

IPv6 独立表同样用 blackhole 默认路由防止 WAN 失效时回落 main。mangle 排除 RouterOS 本机目的和组播，保留 ND/RA 所需路径。

复用 `bypass` instance 和 `bypass-export`，只增加 IPv6 接收过滤及邻居；家庭前缀和代理地址必须在 accept 前排除：

```routeros
/routing filter rule
add chain=bypass-v6 rule="if (dst in 2001::/23 || dst in 2001:db8::/32 || dst in 2002::/16 || dst in 3fff::/20 || dst in 2001:db8:100::/56) { reject }" comment="bgp-bypass reserved6 and home"
add chain=bypass-v6 rule="if (dst in 2000::/3 && dst-len == 128) { accept }; reject" comment="bgp-bypass hosts6"

/routing bgp connection
add name=bypass-v6 instance=bypass local.role=ebgp local.address=fd66::1 remote.address=fd66::2 remote.as=65002 afi=ipv6 connect=no listen=yes hold-time=9s keepalive-time=3s input.filter=bypass-v6 output.filter-chain=bypass-export use-bfd=no
```

### 4.4 Linux IPv6 地址与内核设置

创建 `/etc/systemd/network/30-bgp-bypass.network.d/`，将以下内容保存为其中的 `60-ipv6.conf`。这是原 `.network` 的 drop-in；不要为同一接口新建第二份匹配的 `.network`，networkd 不会合并两份主文件：

```ini
[Network]
Address=fd66::2/64
Address=2001:db8:100:66::2/64
IPv6AcceptRA=no
LinkLocalAddressing=ipv6

[Route]
Destination=::/0
Gateway=fd66::1
PreferredSource=2001:db8:100:66::2
```

静态默认路由不依赖 RA；若改用 RA，forwarding 后还需设置接口 `accept_ra=2` 并核对地址生命周期。保存 `/etc/sysctl.d/90-bgp-bypass6.conf`：

```ini
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.all.accept_redirects=0
```

```sh
sudo networkctl reload
sudo networkctl reconfigure bypass0
sudo sysctl --system
ip -6 -br address show dev bypass0
ip -6 route show default
```

### 4.5 Linux IPv6 nftables 与策略路由

保存 `/etc/nftables.d/bgp-bypass6.nft`。它与 IPv4 文件使用不同的表，各自只处理对应地址族：

```nft
table ip6 bgp_bypass6 {
  chain prerouting {
    type filter hook prerouting priority mangle; policy accept;
    iifname != "bypass0" return
    meta mark & 0xff == 1 return
    ct direction reply return

    # 先接收 DNAT 到本机的 DNS；必须经过 TPROXY 才有 dns_inbounds 元数据。
    ip6 daddr fd66::2 meta l4proto { tcp, udp } th dport 53 tproxy to :12346 meta mark set (meta mark & 0xffffff00) | 2 accept

    # 本机 SSH/健康/BGP 控制流量不劫持；不要全局放过目标端口 22/179 等。
    fib daddr type local return
    meta l4proto { tcp, udp } tproxy to :12346 meta mark set (meta mark & 0xffffff00) | 2 accept
  }

  chain input {
    type filter hook input priority filter; policy drop;
    iifname "lo" accept
    ct state invalid drop
    ct state established,related accept
    meta l4proto ipv6-icmp accept
    iifname "bypass0" meta mark & 0xff == 2 accept
  }

  chain forward {
    type filter hook forward priority filter; policy drop;
    ct state invalid drop
    ct state established,related accept
    # 非 TCP/UDP 报文可回送 RouterOS 独立表，包含 ICMPv6 和 PMTU。
    iifname "bypass0" oifname "bypass0" ip6 saddr 2001:db8:100:88::/64 accept
  }
}
```

IPv6 input 保留 ICMPv6，不能屏蔽 ND、RA、Packet Too Big、Time Exceeded。SSH 和健康接口仍使用第 3 节管理路径，不额外开放 IPv6 管理端口。保存 `/etc/systemd/system/bgp-bypass-network6.service`：

```ini
[Unit]
Description=IPv6 network rules for sing-box BGP bypass
Wants=network-online.target
After=network-online.target nftables.service
Before=sing-box-bypass.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/usr/sbin/ip -6 rule del priority 100 fwmark 2/0xff lookup 100
ExecStartPre=-/usr/sbin/nft delete table ip6 bgp_bypass6
ExecStart=/usr/sbin/ip -6 route replace local ::/0 dev lo table 100
ExecStart=/usr/sbin/ip -6 rule add priority 100 fwmark 2/0xff lookup 100
ExecStart=/usr/sbin/nft -f /etc/nftables.d/bgp-bypass6.nft
ExecStop=-/usr/sbin/nft delete table ip6 bgp_bypass6
ExecStop=-/usr/sbin/ip -6 rule del priority 100 fwmark 2/0xff lookup 100
ExecStop=-/usr/sbin/ip -6 route del local ::/0 dev lo table 100

[Install]
WantedBy=multi-user.target
```

创建 `/etc/systemd/system/sing-box-bypass.service.d/`，保存 `60-ipv6.conf`，在第 5 节基础单元上追加依赖：

```ini
[Unit]
Requires=bgp-bypass-network6.service
After=bgp-bypass-network6.service
```

### 4.6 RouterOS IPv6 防火墙与 FastTrack

合并到现有规则，在相关 drop 前放行，并保留现有安全限制：

```routeros
/ipv6 firewall filter
add chain=input in-interface=ether2 src-address=fd66::2 dst-address=fd66::1 protocol=tcp dst-port=179 action=accept comment="bgp-bypass BGP6"
add chain=input in-interface=bridge-lan dst-address=2001:db8:100:88::1 protocol=udp dst-port=53 action=accept comment="bgp-bypass fallback DNS6 UDP"
add chain=input in-interface=bridge-lan dst-address=2001:db8:100:88::1 protocol=tcp dst-port=53 action=accept comment="bgp-bypass fallback DNS6 TCP"
add chain=forward in-interface=bridge-lan out-interface=ether2 src-address=2001:db8:100:88::/64 action=accept comment="bgp-bypass LAN to Linux6"
add chain=forward in-interface=ether2 out-interface=bridge-lan dst-address=2001:db8:100:88::/64 action=accept comment="bgp-bypass Linux to LAN6"
add chain=forward in-interface=ether2 out-interface=ether1 src-address=2001:db8:100:66::2 action=accept comment="bgp-bypass Linux WAN6"
add chain=forward in-interface=ether2 out-interface=ether1 src-address=2001:db8:100:88::/64 protocol=icmpv6 action=accept comment="bgp-bypass forwarded ICMP6"
```

基础规则应已有 established/related、必要 ICMPv6 和 WAN 入站保护。非 TCP/UDP 协议按需增加转发许可。FastTrack 排除：

```routeros
/ipv6 firewall mangle
add chain=forward in-interface=ether2 connection-mark=no-mark action=mark-connection new-connection-mark=bgp-bypass passthrough=yes comment="bgp-bypass no fasttrack in6"
add chain=forward out-interface=ether2 connection-mark=no-mark action=mark-connection new-connection-mark=bgp-bypass passthrough=yes comment="bgp-bypass no fasttrack out6"

/ipv6 firewall filter set [find where action=fasttrack-connection] connection-mark=no-mark
```

最后一条仅在目标版本支持且启用 IPv6 FastTrack 时执行。已有自定义标记条件需合并，不能覆盖；连接标记仅排除 FastTrack，不能双向设置路由标记。

### 4.7 RouterOS IPv6 DNS 与回切动作

通过 LAN RA RDNSS 公布 RouterOS LAN IPv6 地址。确认有匹配的 ND 项；使用 `interface=all` 默认项时，按现有 RA 设计创建或调整专用于 LAN 的项：

```routeros
/ipv6 nd set [find where interface="bridge-lan"] advertise-dns=yes dns=2001:db8:100:88::1

/ipv6 firewall nat
add chain=dstnat in-interface=bridge-lan dst-address=2001:db8:100:88::1 protocol=udp dst-port=53 action=dst-nat to-address=fd66::2/128 to-ports=53 disabled=yes comment="bgp-bypass-dns"
add chain=dstnat in-interface=bridge-lan dst-address=2001:db8:100:88::1 protocol=tcp dst-port=53 action=dst-nat to-address=fd66::2/128 to-ports=53 disabled=yes comment="bgp-bypass-dns"
```

IPv6 NAT 使用 `to-address` 字段，规则保持禁用，排在已有同目标 DNS redirect/DNAT 前。备用 DNS 复用第 3 节 RouterOS 配置，不依赖 Linux。

注册 IPv6 回切动作，供同一个健康脚本调用。健康探测继续使用共用管理地址，不等于验证了 IPv6 DNS 或数据链路：

```routeros
/system script add name=bgp-bypass-dns6-enable comment="bgp-bypass-dns-enable" policy=read,write,test source={
    /ipv6 firewall nat enable [find where comment="bgp-bypass-dns"];
}
/system script add name=bgp-bypass-dns6-disable comment="bgp-bypass-dns-disable" policy=read,write,test source={
    /ipv6 firewall nat disable [find where comment="bgp-bypass-dns"];
}
```

### 4.8 IPv6 验证与撤销

完成第 5 节启动后，在 Linux 检查：

```sh
ping -6 -c 3 fd66::1
ip -6 route get 2606:4700:4700::1111
ip -6 route get 2001:db8:100:88::100
ip -6 rule show
ip -6 route show table 100
sudo nft list table ip6 bgp_bypass6
```

公网 route get 的 `src` 必须为 Linux 实际公网 IPv6，不能是 `fd66::2`。从 LAN 测试客户端检查 IPv6 DNS 传输：

```sh
dig @fd66::2 github.com A
dig @fd66::2 github.com AAAA
dig +tcp @fd66::2 github.com AAAA
```

选择确有 AAAA 的代理域名，检查 `bypass-v6`、目标 `/128` 和 next-hop `fd66::2`；验证新的 TCP/UDP、大响应、ICMPv6 和 PMTU：

```routeros
/routing bgp session print detail
/ipv6 route print detail where bgp
/ipv6 route print detail where routing-table=bypass-direct
/ipv6 firewall mangle print stats
/ipv6 firewall nat print stats where comment="bgp-bypass-dns"
```

撤销 IPv6 时，先按第 7 节停止共用调度器，再执行：

```routeros
/ipv6 firewall nat disable [find where comment="bgp-bypass-dns"]
/routing bgp connection disable [find where name="bypass-v6"]
```

## 5. 共用进程、健康检查与启用流程

### 5.1 sing-box systemd 单元

保存 `/etc/systemd/system/sing-box-bypass.service`；若已有 sing-box 服务，修改现有单元，不要运行两个实例。IPv6 部署使用第 4 节 drop-in 增加依赖：

```ini
[Unit]
Description=sing-box with GoBGP bypass
Requires=bgp-bypass-network4.service
After=network-online.target bgp-bypass-network4.service

[Service]
Type=simple
User=root
UMask=0077
StateDirectory=sing-box
StateDirectoryMode=0700
ExecStartPre=/usr/local/bin/sing-box check -c /etc/sing-box/config.json
ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box/config.json
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
KillSignal=SIGTERM
LimitNOFILE=65536
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

权限用于 TPROXY、socket mark 等；其他入口/协议按需调整。单元停止顺序保证先停止 sing-box、撤路并关闭会话，再拆网络规则。

安装和基础检查：

```sh
sudo install -m 0755 ./sing-box /usr/local/bin/sing-box
sudo chmod 0600 /etc/sing-box/config.json
sudo /usr/local/bin/sing-box check -c /etc/sing-box/config.json
```

IPv4 文件检查和网络单元启用：

```sh
sudo nft -c -f /etc/nftables.d/bgp-bypass4.nft
sudo systemd-analyze verify /etc/systemd/system/bgp-bypass-network4.service /etc/systemd/system/sing-box-bypass.service
sudo systemctl daemon-reload
sudo systemctl enable --now bgp-bypass-network4.service
```

部署了 IPv6 扩展时，额外执行以下 IPv6 检查和启用：

```sh
sudo nft -c -f /etc/nftables.d/bgp-bypass6.nft
sudo systemd-analyze verify /etc/systemd/system/bgp-bypass-network6.service /etc/systemd/system/sing-box-bypass.service
sudo systemctl daemon-reload
sudo systemctl enable --now bgp-bypass-network6.service
```

最后启动同一个 sing-box 服务：

```sh
sudo systemctl enable --now sing-box-bypass.service
curl -fsS http://127.0.0.1:9098/livez
curl -fsS http://127.0.0.1:9098/dns-readyz
```

如果是在运行中的 IPv4 部署上增加 IPv6，先按第 7 节回切 DNS、撤路并停止 sing-box，再应用 IPv6 文件及 drop-in；检查通过后按本节启动，重新执行对应验收。

### 5.2 共用 DNS 健康脚本

健康接口保持 `192.168.66.2:9099`，只允许 RouterOS 访问；详细诊断仅绑定 loopback 9098。以下脚本只负责探测和调用已注册动作：IPv4 动作在第 3 节，IPv6 动作在第 4 节，未部署的地址族无需注册。

一次探测失败调用所有停用动作，连续两次成功调用所有启用动作。动作按顺序执行，并非事务。创建后 scheduler 保持禁用，验收完成再启用：

```routeros
/system script add name=bgp-bypass-dns-check policy=read,write,test source={
    :global bgpBypassDNSGood;
    :if ([:typeof $bgpBypassDNSGood] = "nothing") do={ :set bgpBypassDNSGood 0; }
    :local ok false;
    :do {
        :local result [/tool fetch url="http://192.168.66.2:9099/dns-readyz" src-address=192.168.66.1 http-method=get http-max-redirect-count=0 idle-timeout=2s duration=3s output=user as-value];
        :if (($result->"status") = "finished") do={
            :if (($result->"data") = "ok\n") do={ :set ok true; }
        }
    } on-error={ :set ok false; }
    :if ($ok) do={
        :if ($bgpBypassDNSGood < 2) do={ :set bgpBypassDNSGood ($bgpBypassDNSGood + 1); }
        :if ($bgpBypassDNSGood >= 2) do={
            :foreach handler in=[/system script find where comment="bgp-bypass-dns-enable"] do={
                /system script run $handler;
            }
        }
    } else={
        :set bgpBypassDNSGood 0;
        :foreach handler in=[/system script find where comment="bgp-bypass-dns-disable"] do={
            /system script run $handler;
        }
    }
}
/system scheduler add name=bgp-bypass-dns-check start-time=startup interval=5s on-event=bgp-bypass-dns-check policy=read,write,test disabled=yes
```

HTTP 非成功响应或超时按失败处理，同时核对正文，不能把 `/livez` 替换进来。`/dns-readyz` 的内部探测只表示内部 DNS 路由可解析根 NS，不验证每个按域名选择的上游、客户端入口或 IPv6 数据链路。真实双栈 DNS 和业务访问还需下面的验收。

**NAT 状态边界：** RouterOS 只在连接首包选择 NAT；切换规则不撤销已存在的 DNS conntrack。失败时新 DNS 流会回到 RouterOS，已建立 TCP DNS 或复用 UDP 源端口的映射可能继续指向 Linux，直到过期或应用重连。若要求更快切换，应按目标版本字段精确删除本方案的 DNS conntrack 项；本文不提供清空全表的脚本，也不承诺无窗口回切。正常恢复后，旧的直连 DNS conntrack 同样可能延迟进入 Linux。

### 5.3 观察、测试通告、接管 DNS

1. DNS DNAT 禁用，`advertise: false`，按第 3.8 / 4.8 节分别验证网络和客户端 DNS。不要用 Linux 本机查询代替客户端入口学习验证。
2. 检查下列诊断。期望路由由有效租约、活动连接引用和固定配置决定；观察模式不建立 BGP，实际通告为空。
3. RouterOS 接收过滤先只放行测试目标，把 `advertise` 改为 `true`，执行 `sing-box check` 后重启服务。分别检查所启用地址族的会话、目标 FIB 和新连接。本地 RIB 成功不等于 RouterOS FIB 成功。
4. 验收后放宽到对应地址族的主机路由过滤范围，手动运行健康脚本验证动作，再启用调度器。

```sh
curl -fsS 'http://127.0.0.1:9098/routes?limit=100'
curl -fsS http://127.0.0.1:9098/metrics
journalctl -u sing-box-bypass.service -n 100 --no-pager
```

```routeros
/system script run bgp-bypass-dns-check
/system scheduler enable [find where name="bgp-bypass-dns-check"]
```

`/routes/{实际IP}` 可查看租约、规则和冲突。若目标版本不支持 `where bgp`，用路由 `print detail` 按前缀查询。3/9 秒是 BGP 计时器，不是端到端恢复保证，还涉及探测周期及 NAT 状态。

## 6. 共用配置参数与验收

所有时间字段均使用 `"250ms"`、`"3s"` 等 duration 字符串。

| 配置路径 | 默认值/约束 | 作用 |
| --- | --- | --- |
| `route.rules[].bgp_bypass` | 旁路服务标签 | 匹配当前目标是否属于本条 `outbound` 的有效学习/固定集合 |
| `services[].type/tag` | `bgp-bypass` / 唯一标签 | 注册控制服务，供出口引用 |
| `advertise` | `false` | 是否建立 BGP 会话并发布路由 |
| `dns_inbounds` | 必填、已有入口标签 | 只学习这些入口的客户端 DNS 最终响应 |
| `state_path` | 不配置则不持久化 | 保存未过期关联，重启按当前原生规则重算；不保存连接 |
| `bgp.local_asn/router_id` | 必填，ASN 非零，Router ID 为 IPv4 | 本地 BGP 身份 |
| `bgp.listen_port` | `-1` | 仅主动连接；1–65535 开启监听，0 无效 |
| `bgp.next_hop_ipv4/next_hop_ipv6` | 对应地址族启用时必填 | 通告的下一跳 |
| `bgp.keepalive/hold_time` | `3s/9s` | 整秒，keepalive 至少 1s，hold 3–65535s，keepalive ≤ hold/3 |
| `bgp.peers[].tag/address/local_address/asn` | 必填 | 邻居标识、对端地址、源地址、对端 ASN；仅 eBGP |
| `bgp.peers[].families` | 必填 | `ipv4-unicast` 或 `ipv6-unicast`，必须与传输地址族相同 |
| `bgp.peers[].password` | 不启用 | TCP MD5 密码；在本文列举的可构建平台中，当前主动拨号实现仅支持 Linux/OpenBSD |
| `persistent_routes[].ip_cidr/rule_set/outbound` | 前两者至少一个，outbound 必填 | 固定代理前缀；规则集必须是纯目标前缀并集 |
| `exclude_prefixes` | 额外排除列表 | 与内置保留地址保护共同生效；固定前缀重叠时报错 |
| `limits.max_dynamic_routes` | `10000` | 动态代理目标数量上限 |
| `limits.max_dns_associations` | `100000` | 域名/IP 租约关联数量上限 |
| `limits.max_pending_updates` | `4096` | 待处理 DNS 响应事件数量上限，不是 IP 数量 |
| `timing.commit_timeout` | `250ms` | 冷学习等待本地提交的预算；超时仍返回 DNS |
| `timing.settle_delay` | `100ms`，允许 `0s` | 新通告提交后的缓冲，不是远端 FIB 确认 |
| `timing.withdraw_delay` | `60s`，允许 `0s` | 租约和活动引用全部释放后的撤路等待 |
| `observability.listen/listen_port` | `127.0.0.1/0` | 主观测地址；端口 0 关闭 |
| `observability.metrics/diagnostics` | 均为 `false` | 开启指标、分页路由及 IP 解释接口 |
| `observability.secret` | 空 | 主观测端口 Bearer Token，非 loopback 监听必填 |
| `observability.health_listen/health_port` | 端口 0 关闭 | 独立健康端口；启用时地址必填，仅提供健康路径 |

原生业务规则按顺序首匹配，保留 sing-box 本身的条件组合语义。DNS 预判仅使用域名、目标 IP、IP 版本、规则集及其逻辑组合；`inbound` 条件须对全部 `dns_inbounds` 有相同的包含关系。含端口、网络类型、源地址、进程、嗅探等不可用条件的整条规则跳过，逻辑规则不拆分或放宽；嗅探、解析、DNS 劫持、route-options、bypass 及改写目标地址/端口的动作不执行。可评估的 reject 规则表示不引流，不在 DNS 阶段执行拒绝。没有匹配时使用原生默认出口。

可评估规则引用的规则集若含不可用条件，启动或热更新会报错。动态集合接入规则始终排除在预判之外，防止学习结果反过来影响学习。启动日志和 `/routes` 的 `dns_preflight_skipped_rules` 列出被跳过的原生规则序号（从 0 开始）。观察模式仍检查完整配置。

固定路由最长前缀匹配，相同前缀不同出口报错。固定前缀若短于主机路由，需在对应地址族的 RouterOS 接收过滤器中明确放行该前缀和长度，不能开放所有长度。不要让代理启动 DNS 依赖固定代理路由。

| 验证 | 预期 |
| --- | --- |
| 普通直连目标 | 不产生动态代理路由，仍走 WAN |
| 代理域名 A/AAAA | 只发布对应主机前缀，TCP/UDP 使用预期逻辑出口 |
| DNS 缓存命中 | 使用剩余 TTL，学习发生在最终响应返回前 |
| TTL 到期、无连接 | 延迟撤路；旧连接仍存在时按活动引用保留 |
| 代理出口不可用但 BGP 正常 | 不因代理故障撤路；节点选择由原生出口组负责，连接可能失败 |
| 正常停止/强制终止/断邻居 | 主路由清除旁路路由，DNS 回切行为符合 NAT 状态边界 |
| 重启 | 仅恢复有效关联，按当前原生路由策略重新发布 |
| FastTrack | 旁路连接不 FastTrack，普通直连仍可 FastTrack |
| IPv6 | ND/RA、Packet Too Big、TCP/UDP、非代理 ICMPv6 均正常 |
| 压力 | 监控路由、关联、队列、拒绝数、CPU/RSS，不能用单次成功代替性能验收 |

主要指标：`sing_box_bgp_bypass_desired_routes`、`actual_routes`、`dns_associations`、`pending_updates`、`queue_rejected_total`、`capacity_rejected_total`、`bgp_errors_total`、`peer_up`。后列短名均带相同前缀。诊断显示域名/IP，指标标签不包含高基数的域名/IP。

## 7. 共用回滚顺序

先停止自动切换，避免手动回切 DNS 后又被重新启用：

```routeros
/system scheduler disable [find where name="bgp-bypass-dns-check"]
```

然后执行第 3.8 节 IPv4 撤销命令；部署了 IPv6 时再执行第 4.8 节 IPv6 撤销命令。确认目标路由已经从 RouterOS FIB 清除，按需精确处理残留 DNS conntrack，再停止进程：

```sh
sudo systemctl disable --now sing-box-bypass.service
```

拆除 IPv4 网络规则：

```sh
sudo systemctl disable --now bgp-bypass-network4.service
```

部署了 IPv6 时，再拆除 IPv6 网络规则：

```sh
sudo systemctl disable --now bgp-bypass-network6.service
```

也可保留网络和服务，将 `advertise` 改为 `false` 后重启继续观察。仅撤销一个地址族时，还需移除该地址族的 sing-box 入口、路由引用、BGP 配置、systemd 依赖和 DNS 回切动作，检查通过后才能重新启用共用调度器。

确认没有流量依赖后再清理地址、路由和防火墙；恢复 FastTrack、DNS 等设置时依据部署前导出。不要先退二进制或拆 TPROXY，而让主路由仍将流量送往旁路。

## 8. 参考与维护

- [配置结构](../../option/bgp_bypass.go)、[默认值和校验](config.go)、[DNS 学习](dns.go)、[原生路由预判](routing.go)、[动态集合匹配](../../route/rule/rule_item_bgp_bypass.go)、[路由状态](state.go)、[BGP 后端](backend.go)。
- [网络集成脚本](../../.github/tests/bgp_network.py) 与 [CI](../../.github/workflows/bgp.yml)；脚本存在不代表已在本机运行或替代 RouterOS 验收。
- [RouterOS BGP instance](https://manual.mikrotik.com/docs/cli-reference/routing/bgp/instance/)、[connection](https://manual.mikrotik.com/docs/cli-reference/routing/bgp/connection/)、[template](https://manual.mikrotik.com/docs/cli-reference/routing/bgp/template/)。
- [RouterOS IPv6 NAT](https://manual.mikrotik.com/docs/cli-reference/ipv6/firewall/nat/)、[IPv6 mangle](https://manual.mikrotik.com/docs/cli-reference/ipv6/firewall/mangle/)、[Fetch](https://help.mikrotik.com/docs/spaces/ROS/pages/8978514/Fetch)。

更新配置契约时同步维护本文及 `docs/examples/bgp-bypass.json`。Linux 文件语法检查、sing-box 配置检查、RouterOS CLI 导入验证和真实转发验收是不同层次，交付时分别记录结果。
