package route

import (
	"strconv"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

const maxBGPBypassRetries = 8

// ValidateConfiguration runs after services and outbounds have been constructed,
// so sing-box check catches invalid dynamic matches without starting listeners.
func (r *Router) ValidateConfiguration(options option.Options) error {
	if options.Route == nil {
		return nil
	}
	manager := service.FromContext[adapter.ServiceManager](r.ctx)
	for i, raw := range options.Route.Rules {
		if raw.Type == C.RuleTypeLogical || raw.DefaultOptions.BGPBypass == "" {
			continue
		}
		tag := raw.DefaultOptions.BGPBypass
		if manager == nil {
			return E.New("missing service manager for bgp_bypass")
		}
		component, found := manager.Get(tag)
		if !found {
			return E.New("rule[", i, "]: unknown bgp-bypass service ", strconv.Quote(tag))
		}
		if _, ok := component.(adapter.BGPBypassAcquirer); !ok {
			return E.New("rule[", i, "]: service does not support bgp_bypass: ", tag)
		}
		outbound, found := r.outbound.Outbound(raw.DefaultOptions.RouteOptions.Outbound)
		if !found {
			return E.New("rule[", i, "]: unknown bgp_bypass outbound")
		}
		if outbound.Type() == C.TypeDirect || outbound.Type() == C.TypeBlock || outbound.Type() == C.TypeDNS {
			return E.New("rule[", i, "]: bgp_bypass requires a proxy or group outbound")
		}
	}
	return nil
}
