package constant

const (
	MetricNamePrefix          = "sing_box_"
	MetricNamePrefixBGPBypass = MetricNamePrefix + "bgp_bypass_"
	MetricNamePrefixCCM       = MetricNamePrefix + "ccm_"
	MetricNamePrefixOCM       = MetricNamePrefix + "ocm_"
	MetricNamePrefixOOM       = MetricNamePrefix + "oom_"
	MetricNamePrefixPower     = MetricNamePrefix + "power_"
	MetricNamePrefixTailscale = MetricNamePrefix + "tailscale_"
)

const (
	MetricNameTrafficBytesTotal          = MetricNamePrefix + "traffic_bytes_total"
	MetricNameTrafficPacketsTotal        = MetricNamePrefix + "traffic_packets_total"
	MetricNameFlowBytesTotal             = MetricNamePrefix + "flow_bytes_total"
	MetricNameFlowPacketsTotal           = MetricNamePrefix + "flow_packets_total"
	MetricNameFlowEstablishmentTotal     = MetricNamePrefix + "flow_establishment_total"
	MetricNameFlowEstablishmentDuration  = MetricNamePrefix + "flow_establishment_duration_seconds"
	MetricNameConnectionsOpenedTotal     = MetricNamePrefix + "connections_opened_total"
	MetricNameConnectionsClosedTotal     = MetricNamePrefix + "connections_closed_total"
	MetricNameConnections                = MetricNamePrefix + "connections"
	MetricNameConnectionDuration         = MetricNamePrefix + "connection_duration_seconds"
	MetricNameDNSRequestsTotal           = MetricNamePrefix + "dns_requests_total"
	MetricNameDNSRequestDuration         = MetricNamePrefix + "dns_request_duration_seconds"
	MetricNameDNSCacheTotal              = MetricNamePrefix + "dns_cache_total"
	MetricNameDialTotal                  = MetricNamePrefix + "dial_total"
	MetricNameDialDuration               = MetricNamePrefix + "dial_duration_seconds"
	MetricNameOutboundGroupSelected      = MetricNamePrefix + "outbound_group_selected"
	MetricNameOutboundDelay              = MetricNamePrefix + "outbound_delay_seconds"
	MetricNameOutboundDelayTimestamp     = MetricNamePrefix + "outbound_delay_timestamp_seconds"
	MetricNameEndpointState              = MetricNamePrefix + "endpoint_state"
	MetricNameEndpointMTUBytes           = MetricNamePrefix + "endpoint_mtu_bytes"
	MetricNameEndpointConnectedTimestamp = MetricNamePrefix + "endpoint_connected_timestamp_seconds"
)

const (
	MetricNameBGPBypassDNSObserve       = MetricNamePrefixBGPBypass + "dns_observe_seconds"
	MetricNameBGPBypassPeerUp           = MetricNamePrefixBGPBypass + "peer_up"
	MetricNameCCMPeriodStartTimestamp   = MetricNamePrefixCCM + "period_start_timestamp_seconds"
	MetricNameOCMPeriodStartTimestamp   = MetricNamePrefixOCM + "period_start_timestamp_seconds"
	MetricNameOOMPressureTotal          = MetricNamePrefixOOM + "pressure_total"
	MetricNameOOMResetEventsTotal       = MetricNamePrefixOOM + "reset_events_total"
	MetricNamePowerCPUQoS               = MetricNamePrefixPower + "cpu_qos_seconds"
	MetricNamePowerInterfaceBytes       = MetricNamePrefixPower + "interface_bytes"
	MetricNamePowerInterfacePackets     = MetricNamePrefixPower + "interface_packets"
	MetricNamePowerTrafficInBytesTotal  = MetricNamePrefixPower + "traffic_in_bytes_total"
	MetricNamePowerTrafficOutBytesTotal = MetricNamePrefixPower + "traffic_out_bytes_total"
	MetricNamePowerTrafficDialsTotal    = MetricNamePrefixPower + "traffic_dials_total"
	MetricNameUSBIPDevices              = MetricNamePrefix + "usbip_devices"
)

// Preserve the upstream Tailscale names and traffic accounting semantics.
const (
	MetricNameTailscaleInboundBytesTotal    = "tailscaled_inbound_bytes_total"
	MetricNameTailscaleOutboundBytesTotal   = "tailscaled_outbound_bytes_total"
	MetricNameTailscaleInboundPacketsTotal  = "tailscaled_inbound_packets_total"
	MetricNameTailscaleOutboundPacketsTotal = "tailscaled_outbound_packets_total"
)

const (
	MetricLabelScope     = "scope"
	MetricLabelKind      = "kind"
	MetricLabelTag       = "tag"
	MetricLabelNetwork   = "network"
	MetricLabelDirection = "direction"
	MetricLabelServer    = "server"
	MetricLabelQueryType = "query_type"
	MetricLabelResult    = "result"
	MetricLabelOutbound  = "outbound"
	MetricLabelNode      = "node"
	MetricLabelService   = "service"
	MetricLabelUser      = "user"
	MetricLabelType      = "type"
	MetricLabelEndpoint  = "endpoint"
	MetricLabelState     = "state"
	MetricLabelReason    = "reason"
	MetricLabelPeer      = "peer"
	MetricLabelFamily    = "family"
	MetricLabelClass     = "class"
	MetricLabelInterface = "interface"
)

const (
	MetricScopeRoute = "route"
	MetricScopeSSM   = "ssm"
	MetricScopeV2Ray = "v2ray"
)

const (
	MetricTrafficKindAll      = "all"
	MetricTrafficKindInbound  = "inbound"
	MetricTrafficKindOutbound = "outbound"
	MetricTrafficKindNode     = "node"
	MetricTrafficKindUser     = "user"
)

const (
	MetricDirectionUpload   = "upload"
	MetricDirectionDownload = "download"
	MetricDirectionIn       = "in"
	MetricDirectionOut      = "out"
)

const (
	MetricDNSKindExchange = "exchange"
	MetricDNSKindLookup   = "lookup"
	MetricDNSKindUpstream = "upstream"
	MetricDNSKindRefresh  = "refresh"
)

const (
	MetricCacheResultHit   = "hit"
	MetricCacheResultMiss  = "miss"
	MetricCacheResultStale = "stale"
)

const (
	MetricResultSuccess  = "success"
	MetricResultTimeout  = "timeout"
	MetricResultCanceled = "canceled"
	MetricResultError    = "error"
)
