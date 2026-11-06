package route

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	"github.com/stretchr/testify/require"
)

type acquisitionTestService struct {
	boxService.Adapter
	match   func(string) bool
	acquire func(string) (func(), bool)
}

func (*acquisitionTestService) Start(adapter.StartStage, *adapter.Scope) error { return nil }

func (s *acquisitionTestService) MatchBGPBypass(_ netip.Addr, outbound string) bool {
	return s.match(outbound)
}

func (s *acquisitionTestService) AcquireBGPBypass(_ netip.Addr, outbound string) (func(), bool) {
	return s.acquire(outbound)
}

type acquisitionTestServices struct {
	adapter.ServiceManager
	control *acquisitionTestService
}

func (m *acquisitionTestServices) Get(tag string) (adapter.Service, bool) {
	return m.control, tag == m.control.Tag()
}

type acquisitionTestDNS struct{ adapter.DNSRouter }

func (*acquisitionTestDNS) LookupReverseMapping(netip.Addr) (string, bool) { return "", false }

type acquisitionTestTransports struct{ adapter.DNSTransportManager }

func (*acquisitionTestTransports) FakeIP() adapter.FakeIPTransport { return nil }

type acquisitionTestOutbound struct {
	outbound.Adapter
	N.Dialer
	handle func(io.Closer, adapter.InboundContext, N.CloseHandlerFunc)
}

func (o *acquisitionTestOutbound) NewConnection(_ context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	o.handle(conn, metadata, onClose)
}

func (o *acquisitionTestOutbound) NewPacketConnection(_ context.Context, conn N.PacketConn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	o.handle(conn, metadata, onClose)
}

type acquisitionTestOutbounds struct {
	adapter.OutboundManager
	values map[string]*acquisitionTestOutbound
}

func (m *acquisitionTestOutbounds) Outbound(tag string) (adapter.Outbound, bool) {
	o, found := m.values[tag]
	return o, found
}

func (m *acquisitionTestOutbounds) Default() adapter.Outbound { return m.values["direct"] }

func acquisitionRouter(t *testing.T, controller *acquisitionTestService, raw string) *Router {
	t.Helper()
	controller.Adapter = boxService.NewAdapter(C.TypeBGPBypass, "bypass")
	ctx := service.ContextWith[adapter.ServiceManager](context.Background(), &acquisitionTestServices{control: controller})
	outbounds := &acquisitionTestOutbounds{values: map[string]*acquisitionTestOutbound{}}
	for _, tag := range []string{"direct", "proxy", "jp"} {
		outboundType := C.TypeSOCKS
		if tag == "direct" {
			outboundType = C.TypeDirect
		}
		outbounds.values[tag] = &acquisitionTestOutbound{
			Adapter: outbound.NewAdapter(outboundType, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
			handle: func(conn io.Closer, _ adapter.InboundContext, onClose N.CloseHandlerFunc) {
				_ = conn.Close()
				if onClose != nil {
					onClose(nil)
				}
			},
		}
	}
	r := &Router{
		ctx:          ctx,
		logger:       log.NewNOPFactory().Logger(),
		outbound:     outbounds,
		dns:          &acquisitionTestDNS{},
		dnsTransport: &acquisitionTestTransports{},
	}
	var rules []option.Rule
	require.NoError(t, json.Unmarshal([]byte(raw), &rules))
	require.NoError(t, r.Initialize(rules, nil))
	for _, rule := range r.rules {
		require.NoError(t, rule.Start())
		t.Cleanup(func() { _ = rule.Close() })
	}
	return r
}

type acquisitionTestPacketConn struct{ N.PacketConn }

func (*acquisitionTestPacketConn) Close() error { return nil }

func acquisitionMetadata(network string) adapter.InboundContext {
	return adapter.InboundContext{
		Inbound:     "lan",
		Network:     network,
		Destination: M.Socksaddr{Addr: netip.MustParseAddr("8.8.8.8"), Port: 443},
	}
}

func routeAcquisitionConnection(t *testing.T, r *Router, network string) error {
	t.Helper()
	metadata := acquisitionMetadata(network)
	if network == N.NetworkUDP {
		return r.routePacketConnection(context.Background(), &acquisitionTestPacketConn{}, metadata, nil)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	return r.routeConnection(context.Background(), client, metadata, nil)
}

func TestBGPSelectionRetriesAfterLeaseChange(t *testing.T) {
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		for _, change := range []string{"expired", "earlier winner"} {
			t.Run(network+"/"+change, func(t *testing.T) {
				winner := "proxy"
				var acquisitions, active int
				controller := &acquisitionTestService{
					match: func(outbound string) bool { return outbound == winner },
					acquire: func(outbound string) (func(), bool) {
						acquisitions++
						if outbound == "proxy" {
							winner = ""
							if change == "earlier winner" {
								winner = "jp"
							}
							return nil, false
						}
						active++
						return func() { active-- }, true
					},
				}
				r := acquisitionRouter(t, controller, `[
					{"bgp_bypass":"bypass","outbound":"jp"},
					{"bgp_bypass":"bypass","outbound":"proxy"},
					{"ip_cidr":"8.8.8.8/32","outbound":"direct"}
				]`)
				var selected string
				for _, outbound := range r.outbound.(*acquisitionTestOutbounds).values {
					outbound.handle = func(conn io.Closer, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
						selected = metadata.RouteOutbound
						_ = conn.Close()
						if onClose != nil {
							onClose(nil)
						}
					}
				}
				require.NoError(t, routeAcquisitionConnection(t, r, network))
				if change == "expired" {
					require.Equal(t, "direct", selected)
					require.Equal(t, 1, acquisitions)
				} else {
					require.Equal(t, "jp", selected)
					require.Equal(t, 2, acquisitions)
				}
				require.Zero(t, active)
			})
		}
	}
}

func TestBGPReferenceOwnership(t *testing.T) {
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		for _, completion := range []string{"missing outbound", "unsupported network", "handshake failure", "close", "callback", "handoff"} {
			t.Run(network+"/"+completion, func(t *testing.T) {
				var active, released int
				controller := &acquisitionTestService{
					match: func(string) bool { return true },
					acquire: func(string) (func(), bool) {
						active++
						return func() {
							active--
							released++
						}, true
					},
				}
				r := acquisitionRouter(t, controller, `[{"bgp_bypass":"bypass","outbound":"proxy"}]`)
				proxy := r.outbound.(*acquisitionTestOutbounds).values["proxy"]
				var held io.Closer
				var heldOnClose N.CloseHandlerFunc
				proxy.handle = func(conn io.Closer, _ adapter.InboundContext, onClose N.CloseHandlerFunc) {
					require.Equal(t, 1, active)
					switch completion {
					case "handshake failure":
						_ = N.CloseOnHandshakeFailure(conn, onClose, E.New("dial failed"))
					case "close":
						_ = conn.Close()
						_ = conn.Close()
						onClose(nil)
					case "callback":
						onClose(nil)
						onClose(nil)
						_ = conn.Close()
					case "handoff":
						held, heldOnClose = conn, onClose
					default:
						t.Fatal("unexpected handoff")
					}
				}
				switch completion {
				case "missing outbound":
					delete(r.outbound.(*acquisitionTestOutbounds).values, "proxy")
				case "unsupported network":
					otherNetwork := N.NetworkTCP
					if network == N.NetworkTCP {
						otherNetwork = N.NetworkUDP
					}
					proxy.Adapter = outbound.NewAdapter(C.TypeSOCKS, "proxy", []string{otherNetwork}, nil)
				}
				err := routeAcquisitionConnection(t, r, network)
				if completion == "missing outbound" || completion == "unsupported network" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				if completion == "handoff" {
					require.Equal(t, 1, active, "routing must transfer rather than release the reference")
					require.Zero(t, released)
					require.NoError(t, held.Close())
					heldOnClose(nil)
				}
				require.Zero(t, active)
				require.Equal(t, 1, released)
			})
		}
	}
}

func TestBGPPreMatchDoesNotAcquire(t *testing.T) {
	controller := &acquisitionTestService{
		match: func(string) bool { return true },
		acquire: func(string) (func(), bool) {
			t.Fatal("PreMatch acquired a connection reference")
			return nil, false
		},
	}
	r := acquisitionRouter(t, controller, `[{"bgp_bypass":"bypass","outbound":"proxy"}]`)
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		require.Equal(t, adapter.PreMatchContinue, r.PreMatch(acquisitionMetadata(network), nil).Action)
	}
}

func TestBGPSelectionRetryIsBounded(t *testing.T) {
	var attempts int
	controller := &acquisitionTestService{
		match: func(string) bool { return true },
		acquire: func(string) (func(), bool) {
			attempts++
			return nil, false
		},
	}
	r := acquisitionRouter(t, controller, `[{"bgp_bypass":"bypass","outbound":"proxy"}]`)
	require.ErrorContains(t, routeAcquisitionConnection(t, r, N.NetworkTCP), "changed repeatedly")
	require.Equal(t, 8, attempts)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	metadata := acquisitionMetadata(N.NetworkUDP)
	err := r.routePacketConnection(ctx, &acquisitionTestPacketConn{}, metadata, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 9, attempts)
}
