package bgpbypass

import (
	"context"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"
)

type decision struct {
	Outbound string
	Priority int
}

type learnedRule struct {
	rule     adapter.Rule
	decision decision
}

type compiledConfig struct {
	options   option.BGPBypassServiceOptions
	rules     []learnedRule
	final     decision
	inbound   string
	skipped   []int
	excluded  []netip.Prefix
	sets      map[string]adapter.RuleSet
	fixedSets map[string]bool
	families  map[int]bool
	fixed     map[netip.Prefix]string
	outbounds []string
}

func normalize(o *option.BGPBypassServiceOptions) error {
	if len(o.DNSInbounds) == 0 {
		return E.New("dns_inbounds is required")
	}
	if o.Limits.MaxDynamicRoutes == 0 {
		o.Limits.MaxDynamicRoutes = 10000
	}
	if o.Limits.MaxDNSAssociations == 0 {
		o.Limits.MaxDNSAssociations = 100000
	}
	if o.Limits.MaxPendingUpdates == 0 {
		o.Limits.MaxPendingUpdates = 4096
	}
	if o.Limits.MaxDynamicRoutes < 1 || o.Limits.MaxDNSAssociations < 1 || o.Limits.MaxPendingUpdates < 1 {
		return E.New("limits must be positive")
	}
	if o.Timing.CommitTimeout == 0 {
		o.Timing.CommitTimeout = badoption.Duration(250 * time.Millisecond)
	}
	if o.Timing.SettleDelay == nil {
		d := badoption.Duration(100 * time.Millisecond)
		o.Timing.SettleDelay = &d
	}
	if o.Timing.WithdrawDelay == nil {
		d := badoption.Duration(60 * time.Second)
		o.Timing.WithdrawDelay = &d
	}
	if o.Timing.CommitTimeout < 0 || *o.Timing.SettleDelay < 0 || *o.Timing.WithdrawDelay < 0 {
		return E.New("timing values must be nonnegative")
	}
	if o.BGP.Keepalive == 0 {
		o.BGP.Keepalive = badoption.Duration(3 * time.Second)
	}
	if o.BGP.HoldTime == 0 {
		o.BGP.HoldTime = badoption.Duration(9 * time.Second)
	}
	if o.BGP.ListenPort == nil {
		p := int32(-1)
		o.BGP.ListenPort = &p
	}
	if *o.BGP.ListenPort < -1 || *o.BGP.ListenPort == 0 || *o.BGP.ListenPort > 65535 {
		return E.New("invalid BGP listen_port")
	}
	if o.BGP.LocalASN == 0 {
		return E.New("local_asn must be nonzero")
	}
	id, err := netip.ParseAddr(o.BGP.RouterID)
	if err != nil || !id.Is4() || id.IsUnspecified() || id.IsMulticast() {
		return E.New("router_id must be a unicast IPv4 address")
	}
	keep, hold := time.Duration(o.BGP.Keepalive), time.Duration(o.BGP.HoldTime)
	if keep < time.Second || hold < 3*time.Second || keep > hold/3 || keep%time.Second != 0 || hold%time.Second != 0 || hold > 65535*time.Second {
		return E.New("invalid BGP keepalive/hold_time")
	}
	if len(o.BGP.Peers) == 0 {
		return E.New("at least one BGP peer is required")
	}
	tags, addresses := map[string]bool{}, map[string]bool{}
	for _, p := range o.BGP.Peers {
		// GoBGP's active dialer only implements TCP MD5 on these systems.
		// Other platforms log a warning and ignore the password; fail explicitly.
		if p.Password != "" && !C.IsLinux && !C.IsOpenbsd {
			return E.New("BGP peer ", strconv.Quote(p.Tag), ": TCP MD5 password is not supported by GoBGP's active dialer on ", runtime.GOOS)
		}
		a, e := netip.ParseAddr(p.Address)
		l, le := netip.ParseAddr(p.LocalAddress)
		if e != nil || le != nil || a.BitLen() != l.BitLen() || a == l || a.Zone() != "" || l.Zone() != "" || !a.IsGlobalUnicast() || !l.IsGlobalUnicast() || p.ASN == 0 || p.ASN == o.BGP.LocalASN {
			return E.New("invalid eBGP peer ", strconv.Quote(p.Tag))
		}
		if p.Tag == "" || tags[p.Tag] || addresses[p.Address] {
			return E.New("duplicate or empty peer tag/address")
		}
		tags[p.Tag] = true
		addresses[p.Address] = true
		if len(p.Families) == 0 {
			return E.New("missing peer families")
		}
		for _, f := range p.Families {
			nh := o.BGP.NextHopIPv4
			bits := 32
			if f == "ipv6-unicast" {
				nh = o.BGP.NextHopIPv6
				bits = 128
			} else if f != "ipv4-unicast" {
				return E.New("unsupported family ", strconv.Quote(f))
			}
			n, e := netip.ParseAddr(nh)
			if e != nil || n.BitLen() != bits || !n.IsGlobalUnicast() || n.Zone() != "" {
				return E.New("invalid next hop for ", f)
			}
			if a.BitLen() != bits {
				return E.New("cross-family BGP transport is not supported")
			}
		}
	}
	obs := &o.Observability
	if obs.Listen == "" {
		obs.Listen = "127.0.0.1"
	}
	if obs.ListenPort != 0 {
		a, e := netip.ParseAddr(obs.Listen)
		if e != nil {
			return e
		}
		if !a.IsLoopback() && obs.Secret == "" {
			return E.New("non-loopback observability requires secret")
		}
	}
	if obs.HealthPort != 0 {
		if _, e := netip.ParseAddr(obs.HealthListen); e != nil {
			return E.Cause(e, "health_listen")
		}
	}
	return nil
}

func compile(_ context.Context, router adapter.Router, o option.BGPBypassServiceOptions) (*compiledConfig, error) {
	c := &compiledConfig{
		options:   o,
		sets:      map[string]adapter.RuleSet{},
		fixedSets: map[string]bool{},
		families:  map[int]bool{},
		fixed:     map[netip.Prefix]string{},
	}
	for _, peer := range o.BGP.Peers {
		for _, family := range peer.Families {
			if family == "ipv4-unicast" {
				c.families[32] = true
			} else {
				c.families[128] = true
			}
		}
	}
	// IPv6 destinations must be in the currently allocated global-unicast
	// block. Keep special-purpose ranges conservative; next hops are separate.
	for _, p := range append([]string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.0.2.0/24",
		"192.88.99.0/24",
		"192.168.0.0/16",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
		"224.0.0.0/3",
		"::/3",
		"4000::/2",
		"8000::/1",
		"2001::/23",
		"2001:db8::/32",
		"2002::/16",
		"3fff::/20",
	}, o.ExcludePrefixes...) {
		prefix, e := netip.ParsePrefix(p)
		if e != nil {
			return nil, e
		}
		c.excluded = append(c.excluded, prefix.Masked())
	}
	for _, p := range o.BGP.Peers {
		for _, a := range []string{p.Address, p.LocalAddress} {
			ip := netip.MustParseAddr(a)
			c.excluded = append(c.excluded, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}
	resolve := func(tags []string, fixed bool) ([]adapter.RuleSet, error) {
		var result []adapter.RuleSet
		for _, tag := range tags {
			set, ok := router.RuleSet(tag)
			if !ok {
				return nil, E.New("unknown rule_set ", strconv.Quote(tag))
			}
			c.sets[tag] = set
			if fixed {
				c.fixedSets[tag] = true
			}
			result = append(result, set)
		}
		return result, nil
	}
	for _, raw := range o.PersistentRoutes {
		if raw.Outbound == "" {
			return nil, E.New("persistent route requires outbound")
		}
		c.outbounds = append(c.outbounds, raw.Outbound)
		if len(raw.IPCIDR) == 0 && len(raw.RuleSet) == 0 {
			return nil, E.New("empty persistent route")
		}
		if _, e := resolve(raw.RuleSet, true); e != nil {
			return nil, e
		}
	}
	slices.Sort(c.outbounds)
	c.outbounds = slices.Compact(c.outbounds)
	return c, nil
}

func (c *compiledConfig) excludedIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || !c.families[ip.BitLen()] {
		return true
	}
	for _, p := range c.excluded {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *compiledConfig) buildFixed(allowUnloaded ...bool) (map[netip.Prefix]string, error) {
	result := map[netip.Prefix]string{}
	add := func(p netip.Prefix, out string) error {
		p = p.Masked()
		if p.Addr().Is4In6() || !c.families[p.Addr().BitLen()] {
			return E.New("unsupported persistent address family: ", p)
		}
		if p.Bits() == 0 {
			return E.New("default route forbidden")
		}
		for _, excluded := range c.excluded {
			if p.Overlaps(excluded) {
				return E.New("persistent prefix ", p, " overlaps excluded ", excluded)
			}
		}
		if old, ok := result[p]; ok && old != out {
			return E.New("conflicting persistent prefix ", p)
		}
		result[p] = out
		return nil
	}
	for _, raw := range c.options.PersistentRoutes {
		for _, text := range raw.IPCIDR {
			p, e := netip.ParsePrefix(text)
			if e != nil {
				return nil, e
			}
			if e = add(p, raw.Outbound); e != nil {
				return nil, e
			}
		}
		for _, tag := range raw.RuleSet {
			set := c.sets[tag]
			if len(allowUnloaded) > 0 && allowUnloaded[0] && set.Metadata() == (adapter.RuleSetMetadata{}) {
				continue
			}
			if !set.Metadata().IPCIDRUnion {
				return nil, E.New("rule_set ", tag, " is not a pure prefix union")
			}
			for _, ips := range set.ExtractIPSet() {
				for _, p := range ips.Prefixes() {
					if e := add(p, raw.Outbound); e != nil {
						return nil, e
					}
				}
			}
		}
	}
	return result, nil
}
