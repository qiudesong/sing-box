package bgpbypass

import (
	"context"
	"net"
	"slices"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-tun"
	N "github.com/sagernet/sing/common/network"
)

func (s *Service) track(metadata adapter.InboundContext, matchedRule adapter.Rule, outbound adapter.Outbound) func() {
	if !s.running.Load() || !s.config.options.Advertise || !slices.Contains(s.config.options.DNSInbounds, metadata.Inbound) || outbound == nil {
		return nil
	}
	if outbound.Type() == C.TypeDirect || outbound.Type() == C.TypeBlock || outbound.Type() == C.TypeDNS {
		return nil
	}
	if rule, ok := matchedRule.(adapter.BGPBypassRule); ok && rule.BGPBypassService() == s.Tag() {
		// The final route candidate already owns this reference.
		return nil
	}
	// Track the native router's selected logical outbound, including an
	// exception placed before a learned-destination rule. Groups retain their
	// logical identity while the native connection path selects the node.
	return s.state.pin(metadata.Destination.Addr, outbound.Tag())
}

func (s *Service) RoutedConnection(_ context.Context, conn net.Conn, m adapter.InboundContext, rule adapter.Rule, outbound adapter.Outbound) net.Conn {
	if release := s.track(m, rule, outbound); release != nil {
		return &trackedConn{Conn: conn, release: release}
	}
	return conn
}

func (s *Service) RoutedPacketConnection(_ context.Context, conn N.PacketConn, m adapter.InboundContext, rule adapter.Rule, outbound adapter.Outbound) N.PacketConn {
	if release := s.track(m, rule, outbound); release != nil {
		return &trackedPacketConn{PacketConn: conn, release: release}
	}
	return conn
}

func (s *Service) RoutedFlow(context.Context, adapter.InboundContext, adapter.Rule, adapter.Outbound) tun.FlowTracker {
	return nil
}

type trackedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
func (c *trackedConn) ReaderReplaceable() bool { return true }
func (c *trackedConn) WriterReplaceable() bool { return true }
func (c *trackedConn) Upstream() any           { return c.Conn }

type trackedPacketConn struct {
	N.PacketConn
	release func()
	once    sync.Once
}

func (c *trackedPacketConn) Close() error {
	c.once.Do(c.release)
	return c.PacketConn.Close()
}
func (c *trackedPacketConn) ReaderReplaceable() bool { return true }
func (c *trackedPacketConn) WriterReplaceable() bool { return true }
func (c *trackedPacketConn) Upstream() any           { return c.PacketConn }
