package powerreport

import (
	"runtime"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
)

func (m *Manager) CollectMetrics(emit func(adapter.Metric), users []string) {
	recorder := m.Recorder()
	if recorder == nil {
		return
	}
	recorder.CollectMetrics(emit, users)
}

func (r *Recorder) CollectMetrics(emit func(adapter.Metric), _ []string) {
	r.access.Lock()
	if !r.started || r.closed {
		r.access.Unlock()
		return
	}
	previous := r.previous
	device := r.lastDeviceState
	deviceValid := r.deviceStateValid
	r.access.Unlock()
	metric := func(name string, kind adapter.MetricType, value float64) {
		emit(adapter.Metric{
			Name:  C.MetricNamePrefixPower + name,
			Type:  kind,
			Value: value,
		})
	}
	metric("dns_queries_total", adapter.MetricCounter, float64(r.dnsQueries.Load()))
	metric("connections_opened_total", adapter.MetricCounter, float64(r.connectionsOpened.Load()))
	metric("network_path_updates_total", adapter.MetricCounter, float64(r.networkPathUpdates.Load()))
	if !previous.at.IsZero() {
		metric("sample_timestamp_seconds", adapter.MetricGauge, float64(previous.at.Unix()))
	}
	if previous.usage.valid {
		metric("cpu_user_seconds", adapter.MetricGauge, float64(previous.usage.userTime)/1e9)
		metric("cpu_system_seconds", adapter.MetricGauge, float64(previous.usage.systemTime)/1e9)
		if runtime.GOOS == "darwin" {
			for class, value := range map[string]int64{
				"default":          previous.usage.qosDefaultTime,
				"maintenance":      previous.usage.qosMaintenanceTime,
				"background":       previous.usage.qosBackgroundTime,
				"utility":          previous.usage.qosUtilityTime,
				"legacy":           previous.usage.qosLegacyTime,
				"user_initiated":   previous.usage.qosUserInitiatedTime,
				"user_interactive": previous.usage.qosUserInteractiveTime,
			} {
				emit(adapter.Metric{
					Name:   C.MetricNamePowerCPUQoS,
					Type:   adapter.MetricGauge,
					Labels: map[string]string{C.MetricLabelClass: class},
					Value:  float64(value) / 1e9,
				})
			}
			metric("cpu_performance_seconds", adapter.MetricGauge, float64(previous.usage.performanceUserTime+previous.usage.performanceSystemTime)/1e9)
			metric("package_idle_wakeups", adapter.MetricGauge, float64(previous.usage.packageIdleWakeups))
			metric("interrupt_wakeups", adapter.MetricGauge, float64(previous.usage.interruptWakeups))
			metric("energy_joules", adapter.MetricGauge, float64(previous.usage.energyNanojoules)/1e9)
			metric("performance_energy_joules", adapter.MetricGauge, float64(previous.usage.performanceEnergyNanojoules)/1e9)
		}
		metric("disk_written_bytes", adapter.MetricGauge, float64(previous.usage.diskBytesWritten))
	}
	if deviceValid {
		metric("battery_percent", adapter.MetricGauge, float64(device.BatteryLevel))
		var lowPower float64
		if device.LowPowerMode {
			lowPower = 1
		}
		metric("low_power_mode", adapter.MetricGauge, lowPower)
	}
	for name, counters := range previous.interfaces {
		for _, direction := range []string{C.MetricDirectionIn, C.MetricDirectionOut} {
			bytes, packets := counters.inBytes, counters.inPackets
			if direction == C.MetricDirectionOut {
				bytes, packets = counters.outBytes, counters.outPackets
			}
			labels := map[string]string{C.MetricLabelInterface: name, C.MetricLabelDirection: direction}
			emit(adapter.Metric{
				Name:   C.MetricNamePowerInterfaceBytes,
				Type:   adapter.MetricGauge,
				Labels: labels,
				Value:  float64(bytes),
			})
			emit(adapter.Metric{
				Name:   C.MetricNamePowerInterfacePackets,
				Type:   adapter.MetricGauge,
				Labels: labels,
				Value:  float64(packets),
			})
		}
	}
	for key, counters := range r.snapshotTrafficLocked() {
		labels := map[string]string{C.MetricLabelKind: string(key.kind), C.MetricLabelTag: key.tag}
		emit(adapter.Metric{
			Name:   C.MetricNamePowerTrafficInBytesTotal,
			Type:   adapter.MetricCounter,
			Labels: labels,
			Value:  float64(counters.inBytes),
		})
		emit(adapter.Metric{
			Name:   C.MetricNamePowerTrafficOutBytesTotal,
			Type:   adapter.MetricCounter,
			Labels: labels,
			Value:  float64(counters.outBytes),
		})
		emit(adapter.Metric{
			Name:   C.MetricNamePowerTrafficDialsTotal,
			Type:   adapter.MetricCounter,
			Labels: labels,
			Value:  float64(counters.dials),
		})
	}
}
