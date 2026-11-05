package v2rayapi

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-tun"
	N "github.com/sagernet/sing/common/network"
)

var (
	_ adapter.ConnectionTracker = (*metricsTracker)(nil)
	_ StatsServiceServer        = (*metricsTracker)(nil)
	_ tun.FlowTracker           = (*metricsFlowTracker)(nil)
)

type metricsTracker struct {
	*StatsService
}

func newMetricsTracker(stats *StatsService) adapter.ConnectionTracker {
	if stats == nil {
		return nil
	}
	return &metricsTracker{StatsService: stats}
}

func (t *metricsTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	conn = t.StatsService.RoutedConnection(ctx, conn, metadata, matchedRule, matchOutbound)
	var metrics adapter.TrafficMetricsObserver = adapter.MetricsObserverFromContext(ctx)
	if metrics == nil {
		return conn
	}
	metadata = t.metricsMetadata(metadata, matchOutbound.Tag())
	if metadata.Inbound == "" && metadata.RouteOutbound == "" && metadata.User == "" {
		return conn
	}
	return metrics.TrackConnection(C.MetricScopeV2Ray, "", conn, metadata)
}

func (t *metricsTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	conn = t.StatsService.RoutedPacketConnection(ctx, conn, metadata, matchedRule, matchOutbound)
	var metrics adapter.TrafficMetricsObserver = adapter.MetricsObserverFromContext(ctx)
	if metrics == nil {
		return conn
	}
	metadata = t.metricsMetadata(metadata, matchOutbound.Tag())
	if metadata.Inbound == "" && metadata.RouteOutbound == "" && metadata.User == "" {
		return conn
	}
	return metrics.TrackPacketConnection(C.MetricScopeV2Ray, "", conn, metadata)
}

func (t *metricsTracker) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	tracker := t.StatsService.RoutedFlow(ctx, metadata, matchedRule, matchOutbound)
	if tracker == nil {
		return nil
	}
	var metrics adapter.TrafficMetricsObserver = adapter.MetricsObserverFromContext(ctx)
	if metrics == nil {
		return tracker
	}
	metricsTracker := metrics.TrackFlow(C.MetricScopeV2Ray, "", t.metricsMetadata(metadata, matchOutbound.Tag()))
	if metricsTracker == nil {
		return tracker
	}
	return &metricsFlowTracker{
		FlowTracker: tracker,
		metrics:     metricsTracker,
	}
}

func (t *metricsTracker) metricsMetadata(metadata adapter.InboundContext, outbound string) adapter.InboundContext {
	if !t.inbounds[metadata.Inbound] {
		metadata.Inbound = ""
	}
	metadata.RouteOutbound = ""
	metadata.OutboundChain = nil
	if t.outbounds[outbound] {
		metadata.RouteOutbound = outbound
	}
	if !t.users[metadata.User] {
		metadata.User = ""
	}
	return metadata
}

type metricsFlowTracker struct {
	tun.FlowTracker
	metrics tun.FlowTracker
}

func (t *metricsFlowTracker) AttachFlow(handle tun.FlowHandle) {
	t.metrics.AttachFlow(handle)
	t.FlowTracker.AttachFlow(handle)
}

func (t *metricsFlowTracker) CountForward(n int) {
	t.metrics.CountForward(n)
	t.FlowTracker.CountForward(n)
}

func (t *metricsFlowTracker) CountReverse(n int) {
	t.metrics.CountReverse(n)
	t.FlowTracker.CountReverse(n)
}

func (t *metricsFlowTracker) FlowEstablished() {
	t.metrics.FlowEstablished()
	t.FlowTracker.FlowEstablished()
}

func (t *metricsFlowTracker) CloseFlow(reason tun.FlowCloseReason) {
	t.metrics.CloseFlow(reason)
	t.FlowTracker.CloseFlow(reason)
}
