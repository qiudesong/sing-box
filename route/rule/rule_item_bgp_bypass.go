package rule

import (
	"context"
	"reflect"
	"strconv"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

type BGPBypassItem struct {
	ctx      context.Context
	tag      string
	outbound string
	matcher  adapter.BGPBypassAcquirer
}

func NewBGPBypassItem(ctx context.Context, options option.DefaultRule) (*BGPBypassItem, error) {
	allowed := option.RawDefaultRule{
		BGPBypass: options.BGPBypass,
		Inbound:   options.Inbound,
		Network:   options.Network,
	}
	if !reflect.DeepEqual(options.RawDefaultRule, allowed) {
		return nil, E.New("bgp_bypass can only be combined with inbound and network conditions")
	}
	if (options.Action != "" && options.Action != C.RuleActionTypeRoute) || options.RouteOptions.Outbound == "" {
		return nil, E.New("bgp_bypass requires an explicit route outbound")
	}
	return &BGPBypassItem{
		ctx:      ctx,
		tag:      options.BGPBypass,
		outbound: options.RouteOptions.Outbound,
	}, nil
}

func (r *BGPBypassItem) Start() error {
	manager := service.FromContext[adapter.ServiceManager](r.ctx)
	if manager != nil {
		if component, found := manager.Get(r.tag); found {
			if matcher, ok := component.(adapter.BGPBypassAcquirer); ok {
				r.matcher = matcher
				return nil
			}
		}
	}
	return E.New("unknown bgp-bypass service ", strconv.Quote(r.tag))
}

func (r *BGPBypassItem) Match(metadata *adapter.InboundContext) bool {
	return r.matcher != nil && r.matcher.MatchBGPBypass(metadata.Destination.Addr, r.outbound)
}

func (r *BGPBypassItem) String() string {
	return "bgp_bypass=" + r.tag + "/" + r.outbound
}

func (r *DefaultRule) BGPBypassService() string {
	if r.bgpBypass == nil {
		return ""
	}
	return r.bgpBypass.tag
}

func (r *DefaultRule) AcquireBGPBypass(metadata *adapter.InboundContext) (func(), bool) {
	if r.bgpBypass == nil {
		return nil, true
	}
	if r.bgpBypass.matcher == nil {
		return nil, false
	}
	return r.bgpBypass.matcher.AcquireBGPBypass(metadata.Destination.Addr, r.bgpBypass.outbound)
}
