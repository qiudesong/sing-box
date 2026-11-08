package oomkiller

import (
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"

	"github.com/stretchr/testify/require"
)

func collectTimerMetrics(timer *adaptiveTimer) map[string]float64 {
	service := &Service{timerConfig: timer.timerConfig, adaptiveTimer: timer}
	values := make(map[string]float64)
	service.CollectMetrics(func(metric adapter.Metric) {
		values[metric.Name] = metric.Value
	}, nil)
	return values
}

func TestMetricsAvailableThresholds(t *testing.T) {
	for _, test := range []struct {
		name         string
		usage        uint64
		safetyMargin uint64
		trigger      uint64
	}{
		{name: "minimum", usage: 64 << 20, trigger: 32 << 20},
		{name: "dynamic", usage: 256 << 20, trigger: 64 << 20},
		{name: "maximum", usage: 1024 << 20, trigger: 128 << 20},
		{name: "zero usage", trigger: 32 << 20},
		{name: "custom", usage: 256 << 20, safetyMargin: 10 << 20, trigger: 10 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			timer := newAdaptiveTimer(log.NewNOPFactory().Logger(), nil, nil, nil, timerConfig{
				policyMode:      policyModeAvailable,
				safetyMargin:    test.safetyMargin,
				hasSafetyMargin: test.safetyMargin > 0,
			})
			sample := memorySample{usage: test.usage, available: test.trigger, availableKnown: true}
			timer.access.Lock()
			timer.state = timer.nextState(sample)
			timer.access.Unlock()
			values := collectTimerMetrics(timer)
			require.EqualValues(t, test.trigger, values[C.MetricNamePrefixOOM+"trigger_bytes"])
			require.EqualValues(t, 2*test.trigger, values[C.MetricNamePrefixOOM+"armed_bytes"])
			require.EqualValues(t, pressureStateTriggered, values[C.MetricNamePrefixOOM+"state"])
		})
	}
}

func TestMetricsAvailableThresholdValidity(t *testing.T) {
	timer := newAdaptiveTimer(log.NewNOPFactory().Logger(), nil, nil, nil, timerConfig{policyMode: policyModeAvailable})
	values := collectTimerMetrics(timer)
	require.NotContains(t, values, C.MetricNamePrefixOOM+"trigger_bytes")
	require.NotContains(t, values, C.MetricNamePrefixOOM+"armed_bytes")
	for _, sample := range []memorySample{
		{usage: 128 << 20, available: 256 << 20, availableKnown: true},
		{usage: 256 << 20, available: 512 << 20, availableKnown: true},
		{usage: 128 << 20},
	} {
		timer.access.Lock()
		timer.state = timer.nextState(sample)
		timer.access.Unlock()
		values = collectTimerMetrics(timer)
		if sample.availableKnown {
			require.EqualValues(t, sample.usage/4, values[C.MetricNamePrefixOOM+"trigger_bytes"])
			require.EqualValues(t, sample.usage/2, values[C.MetricNamePrefixOOM+"armed_bytes"])
		} else {
			require.NotContains(t, values, C.MetricNamePrefixOOM+"trigger_bytes")
			require.NotContains(t, values, C.MetricNamePrefixOOM+"armed_bytes")
		}
	}
}

func TestMetricsLimitThresholds(t *testing.T) {
	for _, mode := range []policyMode{policyModeMemoryLimit, policyModeNetworkExtension} {
		t.Run(mode.String(), func(t *testing.T) {
			timer := newAdaptiveTimer(log.NewNOPFactory().Logger(), nil, nil, nil, timerConfig{
				policyMode:   mode,
				memoryLimit:  100 << 20,
				safetyMargin: 5 << 20,
			})
			// Fixed limits are known even before the first sample.
			values := collectTimerMetrics(timer)
			require.EqualValues(t, 95<<20, values[C.MetricNamePrefixOOM+"trigger_bytes"])
			require.EqualValues(t, 90<<20, values[C.MetricNamePrefixOOM+"armed_bytes"])
			timer.access.Lock()
			timer.state = timer.nextState(memorySample{usage: 96 << 20})
			timer.access.Unlock()
			values = collectTimerMetrics(timer)
			require.EqualValues(t, 95<<20, values[C.MetricNamePrefixOOM+"trigger_bytes"])
			require.EqualValues(t, 90<<20, values[C.MetricNamePrefixOOM+"armed_bytes"])
			require.EqualValues(t, pressureStateTriggered, values[C.MetricNamePrefixOOM+"state"])
		})
	}
}

func TestMetricsThresholdSnapshotConcurrent(t *testing.T) {
	timer := newAdaptiveTimer(log.NewNOPFactory().Logger(), nil, nil, nil, timerConfig{
		policyMode:    policyModeAvailable,
		minInterval:   time.Millisecond,
		armedInterval: time.Second,
		maxInterval:   10 * time.Second,
	})
	samples := []memorySample{
		{usage: 128 << 20, available: 100 << 20, availableKnown: true},
		{usage: 256 << 20, available: 48 << 20, availableKnown: true},
	}
	update := func(sample memorySample) {
		timer.access.Lock()
		timer.state = timer.nextState(sample)
		timer.intervalForState()
		timer.access.Unlock()
	}
	update(samples[0])
	var workers sync.WaitGroup
	workers.Go(func() {
		for index := range 1000 {
			update(samples[index%len(samples)])
		}
	})
	workers.Go(func() {
		for range 1000 {
			values := collectTimerMetrics(timer)
			trigger := values[C.MetricNamePrefixOOM+"trigger_bytes"]
			armed := values[C.MetricNamePrefixOOM+"armed_bytes"]
			state := values[C.MetricNamePrefixOOM+"state"]
			interval := values[C.MetricNamePrefixOOM+"sample_interval_seconds"]
			if state == float64(pressureStateNormal) {
				if trigger != 32<<20 || armed != 64<<20 || (interval != 10 && interval != (2*time.Millisecond).Seconds()) {
					t.Errorf("inconsistent normal snapshot: %v", values)
				}
			} else if state != float64(pressureStateTriggered) || trigger != 64<<20 || armed != 128<<20 || interval != time.Millisecond.Seconds() {
				t.Errorf("inconsistent triggered snapshot: %v", values)
			}
		}
	})
	workers.Wait()
	service := &Service{adaptiveTimer: timer}
	service.CollectMetrics(func(adapter.Metric) {
		// Emitting a snapshot must not hold the timer lock.
		require.True(t, timer.access.TryLock(), "emitting a snapshot must not hold the timer lock")
		timer.access.Unlock()
	}, nil)
}
