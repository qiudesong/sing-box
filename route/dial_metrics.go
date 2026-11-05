package route

import (
	"time"

	"github.com/sagernet/sing-box/adapter"
)

type dialMetrics interface {
	Start() time.Time
	Finish(start time.Time, network, outbound string, err error)
}

var (
	_ dialMetrics = (*observedDialMetrics)(nil)
	_ dialMetrics = nopDialMetrics{}
)

type observedDialMetrics struct {
	observer adapter.DialMetricsObserver
}

func newDialMetricsOrNop(observer adapter.DialMetricsObserver) dialMetrics {
	if observer == nil {
		return nopDialMetrics{}
	}
	return &observedDialMetrics{observer: observer}
}

func (*observedDialMetrics) Start() time.Time {
	return time.Now()
}

func (m *observedDialMetrics) Finish(start time.Time, network, outbound string, err error) {
	m.observer.ObserveDial(network, outbound, time.Since(start), err)
}

type nopDialMetrics struct{}

func (nopDialMetrics) Start() time.Time {
	return time.Time{}
}

func (nopDialMetrics) Finish(time.Time, string, string, error) {
}
