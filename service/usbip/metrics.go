//go:build with_usbip && (linux || (darwin && cgo) || windows)

package usbip

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-usbip"
)

func (s *ServerService) SubscribeMetrics(ctx context.Context, update func([]adapter.Metric)) error {
	s.inner.SubscribeDevices(ctx, func(devices []usbip.ControlDeviceInfo) {
		counts := make(map[usbip.DeviceState]float64)
		for _, device := range devices {
			counts[device.State]++
		}
		snapshot := make([]adapter.Metric, 0, 3)
		for _, state := range []usbip.DeviceState{usbip.DeviceStateIdle, usbip.DeviceStateAttached, usbip.DeviceStateUnavailable} {
			snapshot = append(snapshot, adapter.Metric{
				Name: C.MetricNameUSBIPDevices,
				Type: adapter.MetricGauge,
				Labels: map[string]string{
					C.MetricLabelService: s.Tag(),
					C.MetricLabelState:   state.String(),
				},
				Value: counts[state],
			})
		}
		update(snapshot)
	})
	return ctx.Err()
}
