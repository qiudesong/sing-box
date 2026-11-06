//go:build with_prometheus

package observability

import (
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type metrics struct {
	users                     []string
	traffic                   *prometheus.CounterVec
	packets                   *prometheus.CounterVec
	flowTraffic               *prometheus.CounterVec
	flowPackets               *prometheus.CounterVec
	flowEstablished           *prometheus.CounterVec
	flowEstablishmentDuration *prometheus.HistogramVec
	opened                    *prometheus.CounterVec
	closed                    *prometheus.CounterVec
	active                    *prometheus.GaugeVec
	duration                  *prometheus.HistogramVec
	dns                       *prometheus.CounterVec
	dnsDuration               *prometheus.HistogramVec
	cache                     *prometheus.CounterVec
	dials                     *prometheus.CounterVec
	dialDuration              *prometheus.HistogramVec
}

func newMetrics(manager *Manager) (http.Handler, adapter.MetricsObserver, error) {
	registry := prometheus.NewRegistry()
	labels := connectionLabelNames()
	trafficLabels := trafficLabelNames()
	dnsLabels := dnsLabelNames()
	m := &metrics{
		users: slices.Clone(manager.options.MetricsUsers),
		traffic: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameTrafficBytesTotal,
			Help: "Transferred application bytes; kinds are separate, overlapping aggregations.",
		}, trafficLabels),
		packets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameTrafficPacketsTotal,
			Help: "Transferred datagrams.",
		}, trafficLabels),
		flowTraffic: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameFlowBytesTotal,
			Help: "Observed native flow IP bytes, including network and transport headers.",
		}, trafficLabels),
		flowPackets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameFlowPacketsTotal,
			Help: "Observed native flow packets, before optional forwarding fragmentation.",
		}, trafficLabels),
		flowEstablished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameFlowEstablishmentTotal,
			Help: "Native TCP flow establishment results reported by the flow engine.",
		}, dialLabelNames()),
		flowEstablishmentDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    C.MetricNameFlowEstablishmentDuration,
			Help:    "Time from attaching a native TCP flow until the engine reports establishment or early close.",
			Buckets: durationBuckets,
		}, dialLabelNames()),
		opened: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameConnectionsOpenedTotal,
			Help: "Connections admitted to tracking, including pending handshakes.",
		}, labels),
		closed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameConnectionsClosedTotal,
			Help: "Tracked connections closed.",
		}, labels),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: C.MetricNameConnections,
			Help: "Tracked connections not yet closed, including pending handshakes.",
		}, labels),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    C.MetricNameConnectionDuration,
			Help:    "Time from admission to connection close.",
			Buckets: durationBuckets,
		}, labels),
		dns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameDNSRequestsTotal,
			Help: "DNS logical requests and upstream attempts, counted separately.",
		}, dnsLabels),
		dnsDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    C.MetricNameDNSRequestDuration,
			Help:    "DNS logical request or upstream attempt duration.",
			Buckets: durationBuckets,
		}, dnsLabels),
		cache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameDNSCacheTotal,
			Help: "DNS cache decisions, including stale hits.",
		}, cacheLabelNames()),
		dials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: C.MetricNameDialTotal,
			Help: "Logical outbound connection establishment results.",
		}, dialLabelNames()),
		dialDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    C.MetricNameDialDuration,
			Help:    "Logical outbound connection establishment duration.",
			Buckets: durationBuckets,
		}, dialLabelNames()),
	}
	for _, collector := range []prometheus.Collector{
		collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll)),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.traffic, m.packets, m.opened, m.closed, m.active, m.duration,
		m.flowTraffic, m.flowPackets, m.flowEstablished, m.flowEstablishmentDuration,
		m.dns, m.dnsDuration, m.cache, m.dials, m.dialDuration,
		&moduleCollector{manager: manager},
	} {
		if err := registry.Register(collector); err != nil {
			return nil, nil, E.Cause(err, "register observability collector")
		}
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{}), m, nil
}

// Module availability and user series may change, so this is an unchecked
// collector. Descriptors are cached and SDK validation still runs at gathering.
type moduleCollector struct {
	manager     *Manager
	descriptors sync.Map
}

func (*moduleCollector) Describe(chan<- *prometheus.Desc) {}

func (c *moduleCollector) Collect(ch chan<- prometheus.Metric) {
	emit := func(snapshot adapter.Metric) {
		keys := make([]string, 0, len(snapshot.Labels))
		for key := range snapshot.Labels {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		values := make([]string, len(keys))
		var keyBuilder strings.Builder
		keyBuilder.WriteString(snapshot.Name)
		for i, name := range keys {
			values[i] = snapshot.Labels[name]
			keyBuilder.WriteByte(0)
			keyBuilder.WriteString(name)
		}
		key := keyBuilder.String()
		help := snapshot.Help
		if help == "" {
			help = "Module statistic " + snapshot.Name + "."
		}
		descriptor, loaded := c.descriptors.Load(key)
		if !loaded {
			descriptor, _ = c.descriptors.LoadOrStore(key, prometheus.NewDesc(snapshot.Name, help, keys, nil))
		}
		desc := descriptor.(*prometheus.Desc)
		var metric prometheus.Metric
		var err error
		if snapshot.Type == adapter.MetricHistogram {
			metric, err = prometheus.NewConstHistogram(desc, snapshot.Count, snapshot.Sum, snapshot.Buckets, values...)
		} else {
			kind := prometheus.GaugeValue
			if snapshot.Type == adapter.MetricCounter {
				kind = prometheus.CounterValue
			}
			metric, err = prometheus.NewConstMetric(desc, kind, snapshot.Value, values...)
		}
		if err != nil {
			metric = prometheus.NewInvalidMetric(desc, err)
		}
		ch <- metric
	}
	for _, provider := range c.manager.providers {
		provider.CollectMetrics(emit, c.manager.options.MetricsUsers)
	}
}

func resultLabel(err error) string {
	if err == nil {
		return C.MetricResultSuccess
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return C.MetricResultTimeout
	}
	if errors.Is(err, net.ErrClosed) || E.IsCanceled(err) {
		return C.MetricResultCanceled
	}
	return C.MetricResultError
}

func (m *metrics) ObserveDNS(kind, server string, qtype uint16, rcode int, elapsed time.Duration, err error) {
	queryType := strconv.Itoa(int(qtype))
	result := resultLabel(err)
	if err == nil {
		result = strconv.Itoa(rcode)
	}
	labels := []string{kind, server, queryType, result}
	m.dns.WithLabelValues(labels...).Inc()
	m.dnsDuration.WithLabelValues(labels...).Observe(elapsed.Seconds())
}

func (m *metrics) CountDNSCache(server, result string) {
	m.cache.WithLabelValues(server, result).Inc()
}

func (m *metrics) ObserveDial(network, outbound string, elapsed time.Duration, err error) {
	labels := dialLabelValues(network, outbound, err)
	m.dials.WithLabelValues(labels...).Inc()
	m.dialDuration.WithLabelValues(labels...).Observe(elapsed.Seconds())
}

func (m *metrics) observeFlowEstablishment(network, outbound string, elapsed time.Duration, err error) {
	labels := dialLabelValues(network, outbound, err)
	m.flowEstablished.WithLabelValues(labels...).Inc()
	m.flowEstablishmentDuration.WithLabelValues(labels...).Observe(elapsed.Seconds())
}
