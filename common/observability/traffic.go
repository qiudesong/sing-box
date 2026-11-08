//go:build with_prometheus

package observability

import (
	"context"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"

	"github.com/prometheus/client_golang/prometheus"
)

type trafficBinding struct {
	uplink   N.CountFunc
	downlink N.CountFunc
	close    func()
	dial     func(error)
}

func (m *metrics) bind(scope, tag string, metadata adapter.InboundContext, native bool) trafficBinding {
	type dimension struct{ kind, tag string }
	dimensions := []dimension{{C.MetricTrafficKindAll, tag}}
	if metadata.Inbound != "" {
		dimensions = append(dimensions, dimension{C.MetricTrafficKindInbound, metadata.Inbound})
	}
	if metadata.RouteOutbound != "" {
		dimensions = append(dimensions, dimension{C.MetricTrafficKindOutbound, metadata.RouteOutbound})
	}
	if len(metadata.OutboundChain) > 0 {
		dimensions = append(dimensions, dimension{C.MetricTrafficKindNode, metadata.OutboundChain[len(metadata.OutboundChain)-1].Tag()})
	}
	if metadata.User != "" && slices.Contains(m.users, metadata.User) {
		dimensions = append(dimensions, dimension{C.MetricTrafficKindUser, metadata.User})
	}
	var uplink, downlink []N.CountFunc
	var closed []func()
	started := time.Now()
	traffic, packets := m.traffic, m.packets
	if native {
		traffic, packets = m.flowTraffic, m.flowPackets
	}
	for _, dimension := range dimensions {
		labels := []string{scope, dimension.kind, dimension.tag, metadata.Network}
		up := traffic.WithLabelValues(append(slices.Clone(labels), C.MetricDirectionUpload)...)
		down := traffic.WithLabelValues(append(slices.Clone(labels), C.MetricDirectionDownload)...)
		var upPackets, downPackets prometheus.Counter
		if native || metadata.Network == N.NetworkUDP {
			upPackets = packets.WithLabelValues(append(slices.Clone(labels), C.MetricDirectionUpload)...)
			downPackets = packets.WithLabelValues(append(slices.Clone(labels), C.MetricDirectionDownload)...)
		}
		uplink = append(uplink, func(n int64) {
			up.Add(float64(n))
			if upPackets != nil {
				upPackets.Inc()
			}
		})
		downlink = append(downlink, func(n int64) {
			down.Add(float64(n))
			if downPackets != nil {
				downPackets.Inc()
			}
		})
		active := m.active.WithLabelValues(labels...)
		active.Inc()
		m.opened.WithLabelValues(labels...).Inc()
		counter := m.closed.WithLabelValues(labels...)
		duration := m.duration.WithLabelValues(labels...)
		closed = append(closed, func() {
			active.Dec()
			counter.Inc()
			duration.Observe(time.Since(started).Seconds())
		})
	}
	var closeOnce, dialOnce sync.Once
	return trafficBinding{
		uplink: func(n int64) {
			for _, count := range uplink {
				count(n)
			}
		},
		downlink: func(n int64) {
			for _, count := range downlink {
				count(n)
			}
		},
		close: func() {
			closeOnce.Do(func() {
				for _, close := range closed {
					close()
				}
			})
		},
		dial: func(err error) {
			// The native engine reports establishment only for TCP flows.
			if scope != C.MetricScopeRoute || !native || metadata.Network != N.NetworkTCP {
				return
			}
			dialOnce.Do(func() {
				m.observeFlowEstablishment(metadata.Network, metadata.RouteOutbound, time.Since(started), err)
			})
		},
	}
}

func (m *metrics) RoutedConnection(_ context.Context, conn net.Conn, metadata adapter.InboundContext, _ adapter.Rule, outbound adapter.Outbound) net.Conn {
	metadata.RouteOutbound = outbound.Tag()
	return m.TrackConnection(C.MetricScopeRoute, "", conn, metadata)
}

func (m *metrics) RoutedPacketConnection(_ context.Context, conn N.PacketConn, metadata adapter.InboundContext, _ adapter.Rule, outbound adapter.Outbound) N.PacketConn {
	metadata.RouteOutbound = outbound.Tag()
	return m.TrackPacketConnection(C.MetricScopeRoute, "", conn, metadata)
}

func (m *metrics) RoutedFlow(_ context.Context, metadata adapter.InboundContext, _ adapter.Rule, outbound adapter.Outbound) tun.FlowTracker {
	metadata.RouteOutbound = outbound.Tag()
	// Native flow callbacks receive the selected leaf, while the chain retains
	// the logical route target as its first element.
	if len(metadata.OutboundChain) > 0 {
		metadata.RouteOutbound = metadata.OutboundChain[0].Tag()
	}
	return m.TrackFlow(C.MetricScopeRoute, "", metadata)
}

func (m *metrics) TrackConnection(scope, tag string, conn net.Conn, metadata adapter.InboundContext) net.Conn {
	binding := m.bind(scope, tag, metadata, false)
	return &metricConn{
		ExtendedConn: bufio.NewCounterConn(conn, []N.CountFunc{binding.uplink}, []N.CountFunc{binding.downlink}),
		binding:      binding,
	}
}

func (m *metrics) TrackPacketConnection(scope, tag string, conn N.PacketConn, metadata adapter.InboundContext) N.PacketConn {
	binding := m.bind(scope, tag, metadata, false)
	// CounterPacketConn skips callbacks for zero-length UDP payloads in direct
	// and batch I/O, so traffic_packets_total can undercount valid empty datagrams;
	// byte totals are unaffected. CopyPacket's unwrapped counters can count them,
	// making this limitation depend on the I/O path. Keep the upstream wrapper
	// for compatibility: changing shared callback behavior also affects consumers
	// with side effects, such as powerreport activity tracking; a local replacement
	// would need to preserve batch, waiter and counter-unwrapping support.
	return &metricPacketConn{
		PacketConn: bufio.NewCounterPacketConn(conn, []N.CountFunc{binding.uplink}, []N.CountFunc{binding.downlink}),
		binding:    binding,
	}
}

func (m *metrics) TrackFlow(scope, tag string, metadata adapter.InboundContext) tun.FlowTracker {
	return &metricFlow{metrics: m, scope: scope, tag: tag, metadata: metadata}
}

type metricConn struct {
	N.ExtendedConn
	binding trafficBinding
}

func (c *metricConn) Close() error {
	c.binding.close()
	return c.ExtendedConn.Close()
}

func (c *metricConn) Upstream() any           { return c.ExtendedConn }
func (c *metricConn) ReaderReplaceable() bool { return true }
func (c *metricConn) WriterReplaceable() bool { return true }

type metricPacketConn struct {
	N.PacketConn
	binding trafficBinding
}

func (c *metricPacketConn) Close() error {
	c.binding.close()
	return c.PacketConn.Close()
}

func (c *metricPacketConn) Upstream() any           { return c.PacketConn }
func (c *metricPacketConn) ReaderReplaceable() bool { return true }
func (c *metricPacketConn) WriterReplaceable() bool { return true }

type metricFlow struct {
	metrics  *metrics
	scope    string
	tag      string
	metadata adapter.InboundContext
	binding  trafficBinding
}

func (f *metricFlow) AttachFlow(tun.FlowHandle) {
	f.binding = f.metrics.bind(f.scope, f.tag, f.metadata, true)
}

func (f *metricFlow) CountForward(n int) { f.binding.uplink(int64(n)) }
func (f *metricFlow) CountReverse(n int) { f.binding.downlink(int64(n)) }
func (f *metricFlow) FlowEstablished()   { f.binding.dial(nil) }
func (f *metricFlow) CloseFlow(tun.FlowCloseReason) {
	f.binding.dial(E.New("flow closed before establishment"))
	f.binding.close()
}
