//go:build with_gvisor && with_prometheus && !ts_omit_usermetrics

package tailscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/common/observability"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/tailscale/ipn"
	"github.com/sagernet/tailscale/ipn/ipnstate"
	"github.com/sagernet/tailscale/types/key"
	"github.com/sagernet/tailscale/util/usermetric"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
)

type metricsTestPath struct {
	Path string
}

type metricsTestProvider []adapter.Metric

func (p metricsTestProvider) CollectMetrics(emit func(adapter.Metric), _ []string) {
	for _, metric := range p {
		emit(metric)
	}
}

func newMetricsTestEndpoint() *Endpoint {
	return &Endpoint{
		Adapter:  endpoint.NewAdapter(C.TypeTailscale, "test", nil, nil),
		taildrop: &taildropManager{},
	}
}

func findTestMetric(t *testing.T, snapshot []adapter.Metric, name string, labels map[string]string) adapter.Metric {
	t.Helper()
	for _, metric := range snapshot {
		if metric.Name != name {
			continue
		}
		matches := true
		for key, value := range labels {
			if metric.Labels[key] != value {
				matches = false
				break
			}
		}
		if matches {
			return metric
		}
	}
	t.Fatalf("missing metric %s with labels %v", name, labels)
	return adapter.Metric{}
}

func TestTrafficMetricsUseUpstreamCounters(t *testing.T) {
	registry := new(usermetric.Registry)
	for index, name := range []string{
		C.MetricNameTailscaleInboundBytesTotal, C.MetricNameTailscaleOutboundBytesTotal,
		C.MetricNameTailscaleInboundPacketsTotal, C.MetricNameTailscaleOutboundPacketsTotal,
	} {
		counter := usermetric.NewMultiLabelMapWithRegistry[metricsTestPath](registry, name, "counter", "Upstream counter.")
		counter.Add(metricsTestPath{Path: "direct_ipv4"}, int64(index+1))
		counter.Add(metricsTestPath{Path: "derp"}, int64((index+1)*10))
	}
	registry.NewGauge("unrelated_gauge", "Not exported.").Set(123)
	snapshot, err := collectTrafficMetrics("first", registry)
	require.NoError(t, err)
	require.Len(t, snapshot, 8)
	for _, metric := range snapshot {
		require.Equal(t, adapter.MetricCounter, metric.Type)
		require.Equal(t, "Upstream counter.", metric.Help)
		require.Len(t, metric.Labels, 2)
		require.Equal(t, "first", metric.Labels[C.MetricLabelEndpoint])
	}
	metric := findTestMetric(t, snapshot, C.MetricNameTailscaleInboundBytesTotal, map[string]string{"path": "direct_ipv4"})
	require.EqualValues(t, 1, metric.Value)
	metric = findTestMetric(t, snapshot, C.MetricNameTailscaleOutboundPacketsTotal, map[string]string{"path": "derp"})
	require.EqualValues(t, 40, metric.Value)
	// Each endpoint and publication gets independent labels and values.
	other, err := collectTrafficMetrics("second", registry)
	require.NoError(t, err)
	require.Equal(t, "second", other[0].Labels[C.MetricLabelEndpoint])
	other[0].Labels["path"] = "changed"
	require.Equal(t, "first", snapshot[0].Labels[C.MetricLabelEndpoint])
	require.NotEqual(t, "changed", snapshot[0].Labels["path"])
	// A new upstream instance resets its counters; do not accumulate locally.
	reset, err := collectTrafficMetrics("first", new(usermetric.Registry))
	require.NoError(t, err)
	require.Empty(t, reset)
}

func TestTrafficMetricsRejectNonCounter(t *testing.T) {
	registry := new(usermetric.Registry)
	registry.NewGauge(C.MetricNameTailscaleInboundBytesTotal, "Wrong type.").Set(123)
	snapshot, err := collectTrafficMetrics("test", registry)
	require.ErrorContains(t, err, "expected tailscale counter")
	require.Empty(t, snapshot)
}

func TestTrafficMetricsExporterIsolation(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{
		ListenPort: 9098,
		Metrics:    true,
	}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	for index, tag := range []string{"first", "second"} {
		registry := new(usermetric.Registry)
		counter := usermetric.NewMultiLabelMapWithRegistry[metricsTestPath](registry, C.MetricNameTailscaleInboundBytesTotal, "counter", "Received bytes.")
		counter.Add(metricsTestPath{Path: "derp"}, int64(index+1))
		snapshot, err := collectTrafficMetrics(tag, registry)
		require.NoError(t, err)
		manager.Add(metricsTestProvider(snapshot))
	}
	response := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(response.Body)
	require.NoError(t, err)
	family := families[C.MetricNameTailscaleInboundBytesTotal]
	require.NotNil(t, family)
	require.Len(t, family.GetMetric(), 2)
	values := make(map[string]float64)
	for _, metric := range family.GetMetric() {
		var tag string
		for _, label := range metric.GetLabel() {
			if label.GetName() == C.MetricLabelEndpoint {
				tag = label.GetValue()
			} else {
				require.Equal(t, "path", label.GetName())
				require.Equal(t, "derp", label.GetValue())
			}
		}
		require.NotNil(t, metric.GetCounter())
		values[tag] = metric.GetCounter().GetValue()
	}
	require.Equal(t, map[string]float64{"first": 1, "second": 2}, values)
}

func TestStatusMetricsRetainUpstreamFields(t *testing.T) {
	endpoint := newMetricsTestEndpoint()
	endpoint.taildrop.incoming = map[taildropIncomingKey]*taildropIncomingFile{{}: {}}
	endpoint.taildrop.unreadFiles = map[string]bool{"file": true}
	status := &ipnstate.Status{
		BackendState: ipn.Running.String(),
		CertDomains:  []string{"test.ts.net"},
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {Online: true, Active: true, RxBytes: 100, TxBytes: 200},
			key.NewNode().Public(): {Online: false, Active: true, Expired: true},
		},
	}
	snapshot := endpoint.collectStatusMetrics(status)
	for _, metric := range snapshot {
		require.Equal(t, adapter.MetricGauge, metric.Type)
		require.Equal(t, map[string]string{C.MetricLabelEndpoint: "test"}, metric.Labels)
		require.NotEqual(t, C.MetricNamePrefixTailscale+"received_bytes", metric.Name)
		require.NotEqual(t, C.MetricNamePrefixTailscale+"sent_bytes", metric.Name)
	}
	for name, expected := range map[string]float64{
		"up": 1, "peers": 2, "peers_online": 1, "peers_active": 2, "peers_expired": 1,
		"files_waiting": 0, "files_receiving": 1, "files_unread": 1, "certificate_domains": 1,
	} {
		metric := findTestMetric(t, snapshot, C.MetricNamePrefixTailscale+name, nil)
		require.Equal(t, expected, metric.Value, name)
	}
}

func TestMetricsRefreshWithoutStatusEvents(t *testing.T) {
	endpoint := newMetricsTestEndpoint()
	registry := new(usermetric.Registry)
	counter := usermetric.NewMultiLabelMapWithRegistry[metricsTestPath](registry, C.MetricNameTailscaleInboundBytesTotal, "counter", "Received bytes.")
	path := metricsTestPath{Path: "direct_ipv4"}
	counter.Add(path, 10)
	var status atomic.Pointer[ipnstate.Status]
	status.Store(&ipnstate.Status{
		BackendState: ipn.Running.String(),
		Peer: map[key.NodePublic]*ipnstate.PeerStatus{
			key.NewNode().Public(): {Online: true, Active: true, RxBytes: 9999},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	snapshots := make(chan []adapter.Metric, 1)
	done := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Go(func() {
		done <- endpoint.subscribeMetrics(ctx, status.Load, registry, func(snapshot []adapter.Metric) {
			select {
			case snapshots <- snapshot:
			case <-ctx.Done():
			}
		})
	})
	t.Cleanup(func() {
		cancel()
		workers.Wait()
	})
	next := func() []adapter.Metric {
		select {
		case snapshot := <-snapshots:
			return snapshot
		case <-ctx.Done():
			t.Fatal("timed out waiting for metrics")
			return nil
		}
	}
	first := next()
	require.EqualValues(t, 10, findTestMetric(t, first, C.MetricNameTailscaleInboundBytesTotal, nil).Value)
	require.EqualValues(t, 1, findTestMetric(t, first, C.MetricNamePrefixTailscale+"peers_active", nil).Value)
	// No notification is sent. Removing the peer changes gauges, while the
	// native traffic counter continues increasing independently of peer status.
	counter.Add(path, 7)
	status.Store(&ipnstate.Status{BackendState: ipn.Running.String()})
	second := next()
	require.EqualValues(t, 17, findTestMetric(t, second, C.MetricNameTailscaleInboundBytesTotal, nil).Value)
	require.Zero(t, findTestMetric(t, second, C.MetricNamePrefixTailscale+"peers_active", nil).Value)
	require.EqualValues(t, 10, findTestMetric(t, first, C.MetricNameTailscaleInboundBytesTotal, nil).Value)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("metrics subscription did not exit after cancellation")
	}
}

func TestTrafficMetricsConcurrentUpdates(t *testing.T) {
	registry := new(usermetric.Registry)
	counter := usermetric.NewMultiLabelMapWithRegistry[metricsTestPath](registry, C.MetricNameTailscaleOutboundBytesTotal, "counter", "Sent bytes.")
	path := metricsTestPath{Path: "derp"}
	counter.Add(path, 0)
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 1000 {
			counter.Add(path, 1)
		}
	})
	t.Cleanup(workers.Wait)
	for range 50 {
		snapshot, err := collectTrafficMetrics("test", registry)
		require.NoError(t, err)
		require.Len(t, snapshot, 1)
		require.GreaterOrEqual(t, snapshot[0].Value, float64(0))
	}
	workers.Wait()
	snapshot, err := collectTrafficMetrics("test", registry)
	require.NoError(t, err)
	require.EqualValues(t, 1000, snapshot[0].Value)
}
