package adapter

import (
	"context"
	"net"
	"net/http"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-tun"
	N "github.com/sagernet/sing/common/network"
)

type MetricType uint8

const (
	MetricGauge MetricType = iota
	MetricCounter
	MetricHistogram
)

// Metric is a module's numeric snapshot, independent of the export format.
type Metric struct {
	Name    string
	Help    string
	Type    MetricType
	Labels  map[string]string
	Value   float64
	Count   uint64
	Sum     float64
	Buckets map[float64]uint64
}

// MetricsProvider emits synchronously from a consistent, in-memory snapshot.
type MetricsProvider interface {
	CollectMetrics(emit func(Metric), users []string)
}

type MetricsInitializer interface {
	InitializeMetrics(users []string)
}

// MetricsSubscription publishes cached numeric state until ctx is canceled.
// Published snapshots and their labels must remain immutable.
type MetricsSubscription interface {
	SubscribeMetrics(ctx context.Context, update func([]Metric)) error
}

// HealthChecks must only read cached state. Probes run in the owning module.
type HealthChecks interface {
	HealthChecks() map[string]func() bool
}

// Diagnostics handlers are mounted below /services/{type}/{tag}.
type Diagnostics interface {
	RegisterDiagnostics(mux *http.ServeMux)
}

// TrafficMetricsObserver binds counters when a connection is created, never on
// each byte transfer.
type TrafficMetricsObserver interface {
	TrackConnection(scope, tag string, conn net.Conn, metadata InboundContext) net.Conn
	TrackPacketConnection(scope, tag string, conn N.PacketConn, metadata InboundContext) N.PacketConn
	TrackFlow(scope, tag string, metadata InboundContext) tun.FlowTracker
}

type DNSMetricsObserver interface {
	ObserveDNS(kind, server string, qtype uint16, rcode int, elapsed time.Duration, err error)
	CountDNSCache(server, result string)
}

type DialMetricsObserver interface {
	ObserveDial(network, outbound string, elapsed time.Duration, err error)
}

// MetricsObserver is present in the Box context only when metrics are enabled.
type MetricsObserver interface {
	ConnectionTracker
	TrafficMetricsObserver
	DNSMetricsObserver
	DialMetricsObserver
}

type metricsObserverKey struct{}

func ContextWithMetricsObserver(ctx context.Context, observer MetricsObserver) context.Context {
	return context.WithValue(ctx, metricsObserverKey{}, struct{ observer MetricsObserver }{observer})
}

func MetricsObserverFromContext(ctx context.Context) MetricsObserver {
	if ctx == nil {
		return nil
	}
	if value, ok := ctx.Value(metricsObserverKey{}).(struct{ observer MetricsObserver }); ok {
		return value.observer
	}
	return nil
}

// DNSMetricsContext distinguishes background refreshes from foreground attempts.
type DNSMetricsContext struct{}

func DNSMetricsKind(ctx context.Context) string {
	if ctx.Value(DNSMetricsContext{}) != nil {
		return C.MetricDNSKindRefresh
	}
	return C.MetricDNSKindUpstream
}
