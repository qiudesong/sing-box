package oomkiller

import (
	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
)

func (s *Service) CollectMetrics(emit func(adapter.Metric), _ []string) {
	labels := map[string]string{C.MetricLabelService: s.Tag()}
	metric := func(name string, value float64) {
		emit(adapter.Metric{
			Name:   C.MetricNamePrefixOOM + name,
			Type:   adapter.MetricGauge,
			Labels: labels,
			Value:  value,
		})
	}
	metric("memory_limit_bytes", float64(s.timerConfig.memoryLimit))
	if s.adaptiveTimer != nil {
		timer := s.adaptiveTimer
		timer.access.Lock()
		state, interval := timer.state, timer.currentInterval
		thresholds, thresholdsKnown := timer.metricThresholds, timer.metricThresholdsKnown
		timer.access.Unlock()
		metric("state", float64(state))
		metric("sample_interval_seconds", interval.Seconds())
		if thresholdsKnown {
			metric("trigger_bytes", float64(thresholds.trigger))
			metric("armed_bytes", float64(thresholds.armed))
		}
	}
	if s.recorder == nil {
		return
	}
	s.recorder.access.Lock()
	status := s.recorder.status
	sample := s.recorder.metricSample
	connections := s.recorder.metricConnections
	pressure, resets := s.recorder.metricPressure, s.recorder.metricResets
	s.recorder.access.Unlock()
	metric("memory_bytes", float64(sample.usage))
	metric("connections", float64(connections))
	if sample.availableKnown {
		metric("available_bytes", float64(sample.available))
	}
	emit(adapter.Metric{
		Name:   C.MetricNameOOMPressureTotal,
		Type:   adapter.MetricCounter,
		Labels: labels,
		Value:  float64(pressure),
	})
	emit(adapter.Metric{
		Name:   C.MetricNameOOMResetEventsTotal,
		Type:   adapter.MetricCounter,
		Labels: labels,
		Value:  float64(resets),
	})
	metric("peak_memory_bytes", float64(status.PeakMemory))
	metric("snapshots", float64(status.Snapshots))
	if !status.AvailableKnown {
		return
	}
	metric("minimum_available_bytes", float64(status.MinAvailable))
}
