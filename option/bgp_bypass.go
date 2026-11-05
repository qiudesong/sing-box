package option

import "github.com/sagernet/sing/common/json/badoption"

type BGPBypassServiceOptions struct {
	Advertise        bool                       `json:"advertise,omitempty"`
	DNSInbounds      []string                   `json:"dns_inbounds"`
	StatePath        string                     `json:"state_path,omitempty"`
	BGP              BGPBypassBGPOptions        `json:"bgp"`
	PersistentRoutes []BGPBypassPersistentRoute `json:"persistent_routes,omitempty"`
	ExcludePrefixes  []string                   `json:"exclude_prefixes,omitempty"`
	Limits           BGPBypassLimits            `json:"limits,omitempty"`
	Timing           BGPBypassTiming            `json:"timing,omitempty"`
}

type BGPBypassBGPOptions struct {
	LocalASN    uint32             `json:"local_asn"`
	RouterID    string             `json:"router_id"`
	ListenPort  *int32             `json:"listen_port,omitempty"`
	NextHopIPv4 string             `json:"next_hop_ipv4,omitempty"`
	NextHopIPv6 string             `json:"next_hop_ipv6,omitempty"`
	Keepalive   badoption.Duration `json:"keepalive,omitempty"`
	HoldTime    badoption.Duration `json:"hold_time,omitempty"`
	Peers       []BGPBypassPeer    `json:"peers"`
}

type BGPBypassPeer struct {
	Tag          string   `json:"tag"`
	Address      string   `json:"address"`
	LocalAddress string   `json:"local_address"`
	ASN          uint32   `json:"asn"`
	Families     []string `json:"families"`
	Password     string   `json:"password,omitempty"`
}

type BGPBypassPersistentRoute struct {
	IPCIDR   []string `json:"ip_cidr,omitempty"`
	RuleSet  []string `json:"rule_set,omitempty" reference:"rule_set"`
	Outbound string   `json:"outbound" reference:"outbound"`
}

type BGPBypassLimits struct {
	MaxDynamicRoutes   int `json:"max_dynamic_routes,omitempty"`
	MaxDNSAssociations int `json:"max_dns_associations,omitempty"`
	MaxPendingUpdates  int `json:"max_pending_updates,omitempty"`
}

type BGPBypassTiming struct {
	CommitTimeout badoption.Duration  `json:"commit_timeout,omitempty"`
	SettleDelay   *badoption.Duration `json:"settle_delay,omitempty"`
	WithdrawDelay *badoption.Duration `json:"withdraw_delay,omitempty"`
}
