package observability

import (
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	N "github.com/sagernet/sing/common/network"
)

type metricSubscription struct {
	source adapter.MetricsSubscription
	cache  *cachedMetrics
}

type cachedMetrics struct {
	snapshot atomic.Pointer[[]adapter.Metric]
}

func (c *cachedMetrics) CollectMetrics(emit func(adapter.Metric), _ []string) {
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return
	}
	for _, metric := range *snapshot {
		emit(metric)
	}
}

type outboundMetrics struct {
	outbound adapter.Outbound
	history  *urltest.HistoryStorage
}

func (s *Manager) AddOutbound(outbound adapter.Outbound, history *urltest.HistoryStorage) {
	s.Add(outbound)
	if !s.options.Metrics {
		return
	}
	s.providers = append(s.providers, &outboundMetrics{outbound: outbound, history: history})
}

func (s *outboundMetrics) CollectMetrics(emit func(adapter.Metric), _ []string) {
	if group, ok := s.outbound.(adapter.OutboundGroup); ok {
		for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
			selected := group.Selected(network)
			for _, tag := range group.All() {
				var value float64
				if selected != nil && tag == selected.Tag() {
					value = 1
				}
				emit(adapter.Metric{
					Name: C.MetricNameOutboundGroupSelected,
					Type: adapter.MetricGauge,
					Labels: map[string]string{
						C.MetricLabelOutbound: group.Tag(),
						C.MetricLabelNode:     tag,
						C.MetricLabelNetwork:  network,
					},
					Value: value,
				})
			}
		}
	}
	if s.history == nil {
		return
	}
	history := s.history.LoadURLTestHistory(s.outbound.Tag())
	if history == nil {
		return
	}
	labels := map[string]string{C.MetricLabelOutbound: s.outbound.Tag()}
	emit(adapter.Metric{
		Name:   C.MetricNameOutboundDelay,
		Type:   adapter.MetricGauge,
		Labels: labels,
		Value:  float64(history.Delay) / 1000,
	})
	emit(adapter.Metric{
		Name:   C.MetricNameOutboundDelayTimestamp,
		Type:   adapter.MetricGauge,
		Labels: labels,
		Value:  float64(history.Time.Unix()),
	})
}

type endpointMetrics struct{ endpoint adapter.Endpoint }

func (s *Manager) AddEndpoint(endpoint adapter.Endpoint) {
	s.Add(endpoint)
	if !s.options.Metrics {
		return
	}
	s.providers = append(s.providers, &endpointMetrics{endpoint: endpoint})
}

func (s *endpointMetrics) CollectMetrics(emit func(adapter.Metric), _ []string) {
	var state string
	var mtu uint32
	var since float64
	switch endpoint := s.endpoint.(type) {
	case adapter.OpenVPNEndpoint:
		status := endpoint.OpenVPNStatus()
		state = status.State
		if status.TunnelInfo != nil {
			mtu = status.TunnelInfo.MTU
			since = float64(status.TunnelInfo.ConnectedSince.Unix())
		}
	case adapter.OpenConnectEndpoint:
		status := endpoint.OpenConnectStatus()
		state = status.State
		if status.TunnelInfo != nil {
			mtu = status.TunnelInfo.MTU
			since = float64(status.TunnelInfo.ConnectedSince.Unix())
		}
	default:
		return
	}
	for _, candidate := range []string{adapter.OpenVPNStateConnecting, adapter.OpenVPNStateAuthPending, adapter.OpenVPNStateConnected, adapter.OpenVPNStateError} {
		var value float64
		if state == candidate {
			value = 1
		}
		emit(adapter.Metric{
			Name:   C.MetricNameEndpointState,
			Type:   adapter.MetricGauge,
			Labels: map[string]string{C.MetricLabelType: s.endpoint.Type(), C.MetricLabelEndpoint: s.endpoint.Tag(), C.MetricLabelState: candidate},
			Value:  value,
		})
	}
	labels := map[string]string{C.MetricLabelType: s.endpoint.Type(), C.MetricLabelEndpoint: s.endpoint.Tag()}
	if mtu > 0 {
		emit(adapter.Metric{
			Name:   C.MetricNameEndpointMTUBytes,
			Type:   adapter.MetricGauge,
			Labels: labels,
			Value:  float64(mtu),
		})
	}
	if since <= 0 {
		return
	}
	emit(adapter.Metric{
		Name:   C.MetricNameEndpointConnectedTimestamp,
		Type:   adapter.MetricGauge,
		Labels: labels,
		Value:  since,
	})
}
