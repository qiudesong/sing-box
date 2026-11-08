# Observability

Observability belongs at the top level of the sing-box configuration. It provides a shared metrics endpoint, aggregate health checks, and service diagnostics.

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

All features are disabled by default. Addresses default to `127.0.0.1`; ports default to `0` (disabled). Enabling metrics or diagnostics requires a nonzero `listen_port`.

## Build support

Prometheus uses the official `github.com/prometheus/client_golang` SDK and requires `with_prometheus`. The tag is included in the repository's default build lists; metrics still need to be enabled in configuration. Minimal builds may omit the tag. Configuring `metrics: true` without it fails configuration checking. Health and diagnostics work without Prometheus support.

## Authentication

`secret` enables `Authorization: Bearer <secret>` on every path on the main port. It is required when the main listener is not loopback. Use a trusted management network or TLS reverse proxy for remote collection.

The independent health port does not require authentication and only serves health paths. It never serves metrics or diagnostics. Set `health_listen` to the management address when a remote router needs to probe it.

## HTTP paths

All endpoints accept GET only.

| Path | Description |
| --- | --- |
| `/metrics` | Prometheus exposition from enabled, available modules; main port only, requires `metrics` |
| `/livez` | Box is running |
| `/readyz` | Box has completed startup and all registered module readiness checks pass |
| `/services/{type}/{tag}/livez` | Service liveness, if provided |
| `/services/{type}/{tag}/readyz` | Service readiness, if provided |
| `/services/bgp-bypass/{tag}/dns-readyz` | Cached result of the BGP service's existing DNS probe |
| `/services/bgp-bypass/{tag}/routes` | Paginated BGP routes; requires `diagnostics` |
| `/services/bgp-bypass/{tag}/routes/{ip}` | BGP address explanation; requires `diagnostics` |

Service types match their configuration `type`, including `bgp-bypass`. URL-encode the service tag as one path segment. A missing tag, incorrect service type, or unsupported path returns 404.

Without registered module checks, `/readyz` only checks the Box lifecycle. With BGP enabled, disconnected peers or stale reconciliation can make it return 503. This does not actively test proxy connectivity. DNS probes continue in their owning BGP service; scraping never runs a probe.

The former `services[].observability` configuration and root `/routes` and `/dns-readyz` paths are removed. Move settings to top-level `observability` and update probe URLs.

## Prometheus configuration

```yaml
scrape_configs:
  - job_name: sing-box
    scrape_interval: 15s
    static_configs:
      - targets: ["127.0.0.1:9098"]
```

For authenticated collection, add an `authorization` block with `type: Bearer` and `credentials_file` pointing to the token file. The target must be reachable from the Prometheus server; loopback refers to the server's own network namespace.

## Metrics and aggregation

| Family | Meaning |
| --- | --- |
| `go_*`, `process_*` | SDK Go/runtime and platform-supported process metrics |
| `sing_box_traffic_bytes_total` | Application bytes, labeled by scope, kind, tag, network, direction |
| `sing_box_traffic_packets_total` | Datagrams observed by the socket UDP tracker |
| `sing_box_flow_bytes_total`, `sing_box_flow_packets_total` | Native flow IP bytes and packet observations, including inner IP/transport headers |
| `sing_box_flow_establishment_total`, `sing_box_flow_establishment_duration_seconds` | Establishment or early-close results and duration reported by the native TCP flow engine; UDP flows are excluded |
| `sing_box_connections` | Tracked connections and native flows, including pending handshakes |
| `sing_box_connections_opened_total`, `sing_box_connections_closed_total` | Tracked connection lifecycle |
| `sing_box_connection_duration_seconds` | Time from tracking admission until close |
| `sing_box_dial_total`, `sing_box_dial_duration_seconds` | Logical socket outbound establishment results and duration |
| `sing_box_dns_requests_total`, `sing_box_dns_request_duration_seconds` | Logical DNS requests and upstream attempts, counted separately |
| `sing_box_dns_cache_total` | Cache decisions: hit, stale, miss |
| `sing_box_bgp_bypass_*` | Existing BGP route, peer, rejection, reconciliation, and DNS observation statistics |
| `sing_box_outbound_group_selected` | One-hot current selection by group, node, and network |
| `sing_box_outbound_delay_seconds`, `sing_box_outbound_delay_timestamp_seconds` | Last recorded URLTest result; collection does not trigger testing |
| `sing_box_ccm_*`, `sing_box_ocm_*` | Requests/messages/tokens/cost; persisted totals are gauges, `_total` counters cover observations during this Box lifetime |
| `sing_box_oom_*` | Configured memory policy and cached timer/recorder state |
| `sing_box_power_*` | Existing running power recorder's sampled values and counters |
| `sing_box_endpoint_*`, `sing_box_tailscale_*`, `sing_box_usbip_*` | Available endpoint states and cached status aggregates |
| `tailscaled_inbound_bytes_total`, `tailscaled_outbound_bytes_total`, `tailscaled_inbound_packets_total`, `tailscaled_outbound_packets_total` | Native Tailscale traffic counters, retaining upstream names, accounting and `path` labels, with an added `endpoint` label |

Tailscale metrics are sampled immediately when the subscription starts and refreshed from the local backend every two seconds; scraping only reads the cache. Traffic comes directly from each endpoint's upstream metrics registry, not sums of peer byte counts. `sing_box_tailscale_received_bytes` and `sing_box_tailscale_sent_bytes` are no longer exported. `sing_box_tailscale_peers_active` retains the upstream `Active` field's meaning: currently, a packet was sent to that peer within roughly the last two minutes, independently of its online state. This window is defined by upstream.

OOM `trigger_bytes` and `armed_bytes` use the active memory policy: memory-limit and Network Extension policies compare memory usage against upper thresholds, while the available-memory policy compares remaining available memory against lower thresholds. Available-memory thresholds use the timer's latest valid sample and are omitted until one is available; scraping does not sample system memory.

Traffic `scope` separates `route`, `v2ray`, and `ssm`; these scopes overlap. Within each scope, `kind="all"` is the total; `inbound`, `outbound`, `node`, and `user` are separate overlapping views. Do not sum all scopes or kinds. `outbound` identifies the logical route target and `node` identifies the selected leaf. Socket application bytes exclude tunnel/protocol overhead. Native flow callbacks observe complete inner IP packets; these use the separate `flow_*` families. Counts are taken before optional fragmentation and may precede a failed port write. Keep the socket and native-flow byte series separate when comparing payload rates.

V2Ray's configured statistics dimensions still apply. Its resettable API counters and SSM's clear-on-read counters are independent of the Prometheus totals. The SSM `all` tag combines the service and inbound tags with a colon.

`metrics_users` is an exact username allowlist. Only those usernames receive traffic and usage series. Empty means aggregate-only; aggregates always include all users. Usage gauges and counters use `user=""` for the aggregate. Models and historical billing periods are aggregated rather than becoming labels. Request counts follow successfully parsed usage observations.

Billing period gauges use the `period_` name component and expose only the latest known period. `period_start_timestamp_seconds` identifies its start as a value, not a label. They are omitted when no cycle hint has been observed.

DNS `kind="exchange"` and `kind="lookup"` count completed logical calls. `upstream` counts actual transport attempts, including speculative attempts; `refresh` counts background refresh attempts. Cache hits do not create upstream attempts. DNS result labels are numeric response codes or `error`, `timeout`, `canceled`; query types are numeric, with `0` for a logical Lookup. Cache decisions can include coalesced request rechecks and are not a count of client requests.

New duration histograms use second boundaries `0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, +Inf`. Existing BGP histogram boundaries are unchanged.

Counters may reset when the Box is recreated. Missing modules and unsupported platform statistics are omitted. A scrape never starts profiling, power recording, remote status requests, or URL testing. Subscription-backed metrics are absent until the first snapshot arrives. Domains, addresses, connection IDs, and error text are not metric labels.

```promql
# Routed upload bytes per second, without overlapping dimensions.
sum(rate(sing_box_traffic_bytes_total{scope="route",kind="all",direction="upload"}[5m]))

# Logical DNS exchange P99.
histogram_quantile(0.99, sum by (le) (
  rate(sing_box_dns_request_duration_seconds_bucket{kind="exchange"}[5m])
))
```
