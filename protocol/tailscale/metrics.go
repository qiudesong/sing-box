//go:build with_gvisor && with_prometheus

package tailscale

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/tailscale/ipn"
	"github.com/sagernet/tailscale/ipn/ipnstate"
	"github.com/sagernet/tailscale/util/usermetric"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

var _ adapter.MetricsSubscription = (*Endpoint)(nil)

// SubscribeMetrics samples local state immediately and every two seconds, matching
// the upstream engine watch interval. Traffic uses the upstream registry's counters
// and path labels, not sums of peer byte counts that can fall when peers disappear.
// State gauges retain upstream field meanings, including Active's recent-send
// window. Scrapes only read published snapshots; UI status subscriptions are unchanged.
func (t *Endpoint) SubscribeMetrics(ctx context.Context, update func([]adapter.Metric)) error {
	backend := t.server.ExportLocalBackend()
	return t.subscribeMetrics(ctx, backend.Status, backend.UserMetricsRegistry(), update)
}

func (t *Endpoint) subscribeMetrics(ctx context.Context, status func() *ipnstate.Status, registry *usermetric.Registry, update func([]adapter.Metric)) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		traffic, err := collectTrafficMetrics(t.Tag(), registry)
		if err != nil {
			return E.Cause(err, "collect tailscale traffic metrics")
		}
		snapshot := append(t.collectStatusMetrics(status()), traffic...)
		if err := ctx.Err(); err != nil {
			return err
		}
		update(snapshot)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (t *Endpoint) collectStatusMetrics(status *ipnstate.Status) []adapter.Metric {
	var peers, online, active, expired float64
	for _, peer := range status.Peer {
		peers++
		if peer.Online {
			online++
		}
		if peer.Active {
			active++
		}
		if peer.Expired {
			expired++
		}
	}
	var up float64
	if status.BackendState == ipn.Running.String() {
		up = 1
	}
	values := map[string]float64{
		"up": up, "peers": peers, "peers_online": online, "peers_active": active,
		"peers_expired": expired,
		"files_waiting": float64(t.taildrop.waitingFileCount()), "files_receiving": float64(t.taildrop.receivingFileCount()),
		"files_unread": float64(t.taildrop.unreadFileCount()), "certificate_domains": float64(len(status.CertDomains)),
	}
	snapshot := make([]adapter.Metric, 0, len(values))
	for name, value := range values {
		snapshot = append(snapshot, adapter.Metric{
			Name:   C.MetricNamePrefixTailscale + name,
			Type:   adapter.MetricGauge,
			Labels: map[string]string{C.MetricLabelEndpoint: t.Tag()},
			Value:  value,
		})
	}
	return snapshot
}

func collectTrafficMetrics(endpoint string, registry *usermetric.Registry) ([]adapter.Metric, error) {
	// The registry exposes its counters through an HTTP handler rather than a
	// structured collector. Render it in memory; no listener or HTTP request is sent.
	writer := &metricsResponseWriter{header: make(http.Header)}
	registry.Handler(writer, &http.Request{Method: http.MethodGet})
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(&writer.Buffer)
	if err != nil {
		return nil, err
	}
	var snapshot []adapter.Metric
	for _, name := range []string{
		C.MetricNameTailscaleInboundBytesTotal, C.MetricNameTailscaleOutboundBytesTotal,
		C.MetricNameTailscaleInboundPacketsTotal, C.MetricNameTailscaleOutboundPacketsTotal,
	} {
		family := families[name]
		if family == nil {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metric.GetCounter() == nil {
				return nil, E.New("expected tailscale counter: ", name)
			}
			labels := make(map[string]string, len(metric.GetLabel())+1)
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			labels[C.MetricLabelEndpoint] = endpoint
			snapshot = append(snapshot, adapter.Metric{
				Name:   name,
				Help:   family.GetHelp(),
				Type:   adapter.MetricCounter,
				Labels: labels,
				Value:  metric.GetCounter().GetValue(),
			})
		}
	}
	return snapshot, nil
}

type metricsResponseWriter struct {
	bytes.Buffer
	header http.Header
}

func (w *metricsResponseWriter) Header() http.Header { return w.header }
func (*metricsResponseWriter) WriteHeader(int)       {}
