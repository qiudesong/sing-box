package ssmapi

import (
	"net"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	N "github.com/sagernet/sing/common/network"
)

var _ adapter.SSMTracker = (*metricsTracker)(nil)

type metricsTracker struct {
	adapter.SSMTracker
	metrics adapter.TrafficMetricsObserver
	tag     string
}

func newMetricsTracker(tracker adapter.SSMTracker, metrics adapter.TrafficMetricsObserver, tag string) adapter.SSMTracker {
	if metrics == nil {
		return tracker
	}
	return &metricsTracker{
		SSMTracker: tracker,
		metrics:    metrics,
		tag:        tag,
	}
}

func (t *metricsTracker) TrackConnection(conn net.Conn, metadata adapter.InboundContext) net.Conn {
	conn = t.SSMTracker.TrackConnection(conn, metadata)
	metadata.Network = N.NetworkTCP
	return t.metrics.TrackConnection(C.MetricScopeSSM, t.tag, conn, metadata)
}

func (t *metricsTracker) TrackPacketConnection(conn N.PacketConn, metadata adapter.InboundContext) N.PacketConn {
	conn = t.SSMTracker.TrackPacketConnection(conn, metadata)
	metadata.Network = N.NetworkUDP
	return t.metrics.TrackPacketConnection(C.MetricScopeSSM, t.tag, conn, metadata)
}
