package bgpbypass

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
)

type routeBackend interface {
	Start(context.Context) error
	List() (map[netip.Prefix]bool, error)
	Add([]netip.Prefix) error
	Delete([]netip.Prefix) error
	Peers() map[string]bool
	Close() error
}

type bgpBackend struct {
	server  *server.BgpServer
	options option.BGPBypassBGPOptions
	logger  log.ContextLogger
	ctx     context.Context
}

func newBGPBackend(options option.BGPBypassBGPOptions, logger log.ContextLogger) *bgpBackend {
	return &bgpBackend{options: options, logger: logger}
}

func (b *bgpBackend) Start(ctx context.Context) error {
	b.ctx = ctx
	b.server = server.NewBgpServer(server.LoggerOption(slog.New(bgpLogHandler{b.logger}), new(slog.LevelVar)))
	go b.server.Serve()
	err := b.server.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        b.options.LocalASN,
			RouterId:   b.options.RouterID,
			ListenPort: *b.options.ListenPort,
		},
	})
	if err != nil {
		b.server.Stop()
		return err
	}
	failed := true
	defer func() {
		if failed {
			b.server.Stop()
		}
	}()
	localPolicy := &api.Policy{
		Name: "bgp-bypass-local-only",
		Statements: []*api.Statement{
			{
				Name:       "local",
				Conditions: &api.Conditions{RouteType: api.Conditions_ROUTE_TYPE_LOCAL},
				Actions:    &api.Actions{RouteAction: api.RouteAction_ROUTE_ACTION_ACCEPT},
			},
		},
	}
	err = b.server.AddPolicy(ctx, &api.AddPolicyRequest{Policy: localPolicy})
	if err != nil {
		return err
	}
	for _, direction := range []api.PolicyDirection{api.PolicyDirection_POLICY_DIRECTION_IMPORT, api.PolicyDirection_POLICY_DIRECTION_EXPORT} {
		err = b.server.SetPolicyAssignment(ctx, &api.SetPolicyAssignmentRequest{
			Assignment: &api.PolicyAssignment{
				Name:          "global",
				Direction:     direction,
				Policies:      []*api.Policy{{Name: localPolicy.Name}},
				DefaultAction: api.RouteAction_ROUTE_ACTION_REJECT,
			},
		})
		if err != nil {
			return err
		}
	}

	for _, p := range b.options.Peers {
		var families []*api.AfiSafi
		for _, f := range p.Families {
			afi := api.Family_AFI_IP
			if f == "ipv6-unicast" {
				afi = api.Family_AFI_IP6
			}
			families = append(families, &api.AfiSafi{
				Config: &api.AfiSafiConfig{
					Family:  &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST},
					Enabled: true,
				},
			})
		}
		err = b.server.AddPeer(ctx, &api.AddPeerRequest{
			Peer: &api.Peer{
				Conf: &api.PeerConf{
					NeighborAddress: p.Address,
					PeerAsn:         p.ASN,
					AuthPassword:    p.Password,
				},
				Transport: &api.Transport{LocalAddress: p.LocalAddress},
				AfiSafis:  families,
				Timers: &api.Timers{
					Config: &api.TimersConfig{
						KeepaliveInterval: uint64(time.Duration(b.options.Keepalive) / time.Second),
						HoldTime:          uint64(time.Duration(b.options.HoldTime) / time.Second),
						ConnectRetry:      3,
					},
				},
				GracefulRestart: &api.GracefulRestart{Enabled: false},
			},
		})
		if err != nil {
			return err
		}
	}
	failed = false
	return nil
}

func (b *bgpBackend) path(p netip.Prefix) (*apiutil.Path, error) {
	nlri, err := bgp.NewIPAddrPrefix(p)
	if err != nil {
		return nil, err
	}
	attrs := []bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(bgp.BGP_ORIGIN_ATTR_TYPE_IGP),
		bgp.NewPathAttributeCommunities([]uint32{uint32(bgp.COMMUNITY_NO_EXPORT)}),
	}
	family := bgp.RF_IPv4_UC
	if p.Addr().Is4() {
		nh, e := bgp.NewPathAttributeNextHop(netip.MustParseAddr(b.options.NextHopIPv4))
		if e != nil {
			return nil, e
		}
		attrs = append(attrs, nh)
	} else {
		family = bgp.RF_IPv6_UC
		nh, e := bgp.NewPathAttributeMpReachNLRI(family, []bgp.PathNLRI{{NLRI: nlri}}, netip.MustParseAddr(b.options.NextHopIPv6))
		if e != nil {
			return nil, e
		}
		attrs = append(attrs, nh)
	}
	return &apiutil.Path{
		Family: family,
		Nlri:   nlri,
		Attrs:  attrs,
	}, nil
}

func (b *bgpBackend) Add(prefixes []netip.Prefix) error {
	paths := make([]*apiutil.Path, 0, len(prefixes))
	for _, p := range prefixes {
		path, e := b.path(p)
		if e != nil {
			return e
		}
		paths = append(paths, path)
	}
	_, e := b.server.AddPath(apiutil.AddPathRequest{Paths: paths})
	return e
}

func (b *bgpBackend) Delete(prefixes []netip.Prefix) error {
	paths := make([]*apiutil.Path, 0, len(prefixes))
	for _, p := range prefixes {
		path, e := b.path(p)
		if e != nil {
			return e
		}
		paths = append(paths, path)
	}
	return b.server.DeletePath(apiutil.DeletePathRequest{Paths: paths})
}

func (b *bgpBackend) List() (map[netip.Prefix]bool, error) {
	result := map[netip.Prefix]bool{}
	for _, family := range []bgp.Family{bgp.RF_IPv4_UC, bgp.RF_IPv6_UC} {
		err := b.server.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: family}, func(nlri bgp.NLRI, paths []*apiutil.Path) {
			p, e := netip.ParsePrefix(nlri.String())
			if e != nil {
				return
			}
			for _, path := range paths {
				if (!path.PeerAddress.IsValid() || path.PeerAddress.IsUnspecified()) && !path.Withdrawal {
					result[p.Masked()] = true
				}
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (b *bgpBackend) Peers() map[string]bool {
	result := map[string]bool{}
	for _, p := range b.options.Peers {
		result[p.Tag] = false
	}
	_ = b.server.ListPeer(b.ctx, &api.ListPeerRequest{}, func(p *api.Peer) {
		for _, configured := range b.options.Peers {
			if p.Conf.NeighborAddress == configured.Address {
				result[configured.Tag] = p.State.SessionState == api.PeerState_SESSION_STATE_ESTABLISHED
			}
		}
	})
	return result
}

func (b *bgpBackend) Close() error {
	if b.server == nil {
		return nil
	}
	err := b.server.DeletePath(apiutil.DeletePathRequest{DeleteAll: true})
	b.server.Stop()
	return err
}

type bgpLogHandler struct{ logger log.ContextLogger }

func (h bgpLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h bgpLogHandler) Handle(_ context.Context, r slog.Record) error {
	message := r.Message
	r.Attrs(func(a slog.Attr) bool {
		message += fmt.Sprint(" ", a.Key, "=", a.Value)
		return true
	})
	switch {
	case r.Level >= slog.LevelError:
		h.logger.Error(message)
	case r.Level >= slog.LevelWarn:
		h.logger.Warn(message)
	case r.Level >= slog.LevelInfo:
		h.logger.Info(message)
	default:
		h.logger.Debug(message)
	}
	return nil
}

func (h bgpLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h bgpLogHandler) WithGroup(string) slog.Handler      { return h }
