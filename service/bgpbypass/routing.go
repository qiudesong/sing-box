package bgpbypass

import (
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// previewConditions is deliberately an allowlist. A new upstream condition must
// never silently become a wildcard in DNS preflight. Unsupported rules are
// skipped in their entirety, including logical rules with an unsupported child.
func previewConditions(raw option.Rule) (bool, []string) {
	switch raw.Type {
	case "", C.RuleTypeDefault:
		r := raw.DefaultOptions.RawDefaultRule
		allowed := option.RawDefaultRule{
			Inbound:       r.Inbound,
			IPVersion:     r.IPVersion,
			Domain:        r.Domain,
			DomainSuffix:  r.DomainSuffix,
			DomainKeyword: r.DomainKeyword,
			DomainRegex:   r.DomainRegex,
			IPCIDR:        r.IPCIDR,
			IPIsPrivate:   r.IPIsPrivate,
			RuleSet:       r.RuleSet,
			Invert:        r.Invert,
		}
		return reflect.DeepEqual(r, allowed), r.RuleSet
	case C.RuleTypeLogical:
		var sets []string
		for _, child := range raw.LogicalOptions.Rules {
			ok, childSets := previewConditions(child)
			if !ok {
				return false, nil
			}
			sets = append(sets, childSets...)
		}
		return true, sets
	default:
		return false, nil
	}
}

func (s *Service) compileRouting(options option.Options) error {
	c := s.config
	c.rules = nil
	c.skipped = nil
	c.inbound = c.options.DNSInbounds[0]
	var rawRules []option.Rule
	if options.Route != nil {
		rawRules = options.Route.Rules
	}
	rules := s.router.Rules()
	if len(rules) != len(rawRules) {
		return E.New("router does not expose matching native rules")
	}
	var defaultOutbound string
	if options.Route != nil && options.Route.Final != "" {
		defaultOutbound = options.Route.Final
	} else if outbound := s.outbounds.Default(); outbound != nil {
		defaultOutbound = outbound.Tag()
	}
	// Implicit direct and endpoint defaults are initialized only when the
	// outbound manager starts. Resolve explicit tags without starting it.
	var final string
	if defaultOutbound != "" {
		var err error
		final, err = s.proxyOutbound(defaultOutbound)
		if err != nil {
			return err
		}
	}
	bindings := map[string]bool{}
	for i, raw := range rawRules {
		action := raw.DefaultOptions.RuleAction
		if raw.Type == C.RuleTypeLogical {
			action = raw.LogicalOptions.RuleAction
		}
		if raw.Type != C.RuleTypeLogical && raw.DefaultOptions.BGPBypass != "" {
			r := raw.DefaultOptions
			if r.BGPBypass != s.Tag() {
				continue
			}
			// A binding must cover all learned inbounds and both supported
			// transports; narrower exceptions belong before the binding.
			for _, inbound := range c.options.DNSInbounds {
				if len(r.Inbound) > 0 && !slices.Contains(r.Inbound, inbound) {
					return E.New("bgp_bypass rule[", i, "] does not cover dns_inbound ", inbound)
				}
			}
			if len(r.Network) > 0 && (!slices.Contains(r.Network, N.NetworkTCP) || !slices.Contains(r.Network, N.NetworkUDP)) {
				return E.New("bgp_bypass rule[", i, "] must cover TCP and UDP")
			}
			bindings[r.RouteOptions.Outbound] = true
			continue
		}
		eligible, tags := previewConditions(raw)
		if !eligible || (action.Action != "" && action.Action != C.RuleActionTypeRoute && action.Action != C.RuleActionTypeReject) {
			c.skipped = append(c.skipped, i)
			continue
		}
		if action.RouteOptions.OverrideAddress != "" || action.RouteOptions.OverridePort != 0 {
			c.skipped = append(c.skipped, i)
			continue
		}
		// With no client dimension all configured learning inbounds must
		// produce the same scope decision, even inside an inverted group.
		if err := c.validateInboundScope(raw); err != nil {
			return E.Cause(err, "route rule[", i, "]")
		}
		for _, tag := range tags {
			set, ok := s.router.RuleSet(tag)
			if !ok {
				return E.New("unknown rule_set ", strconv.Quote(tag))
			}
			c.sets[tag] = set
		}
		d := decision{Priority: i}
		if action.Action != C.RuleActionTypeReject {
			out := action.RouteOptions.Outbound
			if out == "" {
				d.Outbound = final
			} else {
				var err error
				d.Outbound, err = s.proxyOutbound(out)
				if err != nil {
					return E.Cause(err, "route rule[", i, "]")
				}
			}
		}
		c.rules = append(c.rules, learnedRule{rule: rules[i], decision: d})
		if d.Outbound != "" {
			c.outbounds = append(c.outbounds, d.Outbound)
		}
	}
	c.final = decision{Outbound: final, Priority: len(rawRules)}
	if final != "" {
		c.outbounds = append(c.outbounds, final)
	}
	slices.Sort(c.outbounds)
	c.outbounds = slices.Compact(c.outbounds)
	for _, tag := range c.outbounds {
		if !bindings[tag] {
			return E.New("missing bgp_bypass route rule for outbound ", strconv.Quote(tag))
		}
	}
	return s.validateOutboundDependencies()
}

func (s *Service) validateOutboundDependencies() error {
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(tag string) error {
		if visiting[tag] {
			return E.New("outbound dependency cycle at ", tag)
		}
		if done[tag] {
			return nil
		}
		outbound, found := s.outbounds.Outbound(tag)
		if !found {
			return E.New("unknown outbound dependency ", tag)
		}
		visiting[tag] = true
		for _, dependency := range outbound.Dependencies() {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		delete(visiting, tag)
		done[tag] = true
		return nil
	}
	for _, tag := range s.config.outbounds {
		if err := visit(tag); err != nil {
			return err
		}
	}
	return nil
}

func (c *compiledConfig) validateInboundScope(raw option.Rule) error {
	if raw.Type == C.RuleTypeLogical {
		for _, child := range raw.LogicalOptions.Rules {
			if err := c.validateInboundScope(child); err != nil {
				return err
			}
		}
		return nil
	}
	inbounds := raw.DefaultOptions.Inbound
	if len(inbounds) == 0 {
		return nil
	}
	first := slices.Contains(inbounds, c.inbound)
	for _, inbound := range c.options.DNSInbounds {
		if slices.Contains(inbounds, inbound) != first {
			return E.New("DNS preflight requires uniform inbound scope across dns_inbounds")
		}
	}
	return nil
}

func (s *Service) proxyOutbound(tag string) (string, error) {
	outbound, found := s.outbounds.Outbound(tag)
	if !found {
		return "", E.New("unknown route outbound ", strconv.Quote(tag))
	}
	if outbound.Type() == C.TypeDirect || outbound.Type() == C.TypeBlock || outbound.Type() == C.TypeDNS {
		return "", nil
	}
	return tag, nil
}

func (c *compiledConfig) decide(domain string, ip netip.Addr) decision {
	metadata := adapter.InboundContext{
		Inbound:     c.inbound,
		Domain:      domain,
		Destination: M.Socksaddr{Addr: ip},
		IPVersion:   6,
	}
	if ip.Is4() {
		metadata.IPVersion = 4
	}
	for _, rule := range c.rules {
		metadata.ResetRuleCache()
		if rule.rule.Match(&metadata) {
			return rule.decision
		}
	}
	return c.final
}

func (s *Service) MatchBGPBypass(ip netip.Addr, outbound string) bool {
	return s.running.Load() && s.configValid.Load() && s.config.options.Advertise &&
		outbound != "" && s.state.lookup(ip, time.Now()) == outbound
}

func (s *Service) AcquireBGPBypass(ip netip.Addr, outbound string) (func(), bool) {
	if !s.running.Load() || !s.configValid.Load() || !s.config.options.Advertise {
		return nil, false
	}
	return s.state.acquire(ip, outbound)
}
