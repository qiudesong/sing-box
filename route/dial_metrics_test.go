package route

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

var _ adapter.DialMetricsObserver = (*recordingDialObserver)(nil)

type dialObservation struct {
	network  string
	outbound string
	elapsed  time.Duration
	err      error
}

type recordingDialObserver struct {
	access sync.Mutex
	calls  []dialObservation
}

func (o *recordingDialObserver) ObserveDial(network, outbound string, elapsed time.Duration, err error) {
	o.access.Lock()
	defer o.access.Unlock()
	o.calls = append(o.calls, dialObservation{
		network:  network,
		outbound: outbound,
		elapsed:  elapsed,
		err:      err,
	})
}

func TestDialMetricsDisabled(t *testing.T) {
	manager := NewConnectionManager(context.Background(), log.NewNOPFactory().Logger())
	start := manager.dialMetrics.Start()
	require.True(t, start.IsZero())
	manager.dialMetrics.Finish(start, N.NetworkTCP, "proxy", context.Canceled)
}

func TestDialMetricsPreserveObservation(t *testing.T) {
	observer := new(recordingDialObserver)
	metrics := newDialMetricsOrNop(observer)
	require.False(t, metrics.Start().IsZero())
	for _, err := range []error{nil, context.Canceled} {
		start := time.Now().Add(-time.Second)
		metrics.Finish(start, N.NetworkUDP, "proxy", err)
	}
	require.Len(t, observer.calls, 2)
	for _, call := range observer.calls {
		require.Equal(t, N.NetworkUDP, call.network)
		require.Equal(t, "proxy", call.outbound)
		require.GreaterOrEqual(t, call.elapsed, time.Second)
	}
	require.NoError(t, observer.calls[0].err)
	require.ErrorIs(t, observer.calls[1].err, context.Canceled)
}

func TestDialMetricsConcurrentObservations(t *testing.T) {
	observer := new(recordingDialObserver)
	metrics := newDialMetricsOrNop(observer)
	var workers sync.WaitGroup
	for index := range 16 {
		workers.Go(func() {
			start := time.Now().Add(-time.Duration(index+1) * time.Second)
			metrics.Finish(start, N.NetworkTCP, strconv.Itoa(index), nil)
		})
	}
	workers.Wait()
	require.Len(t, observer.calls, 16)
	for _, call := range observer.calls {
		index, err := strconv.Atoi(call.outbound)
		require.NoError(t, err)
		require.GreaterOrEqual(t, call.elapsed, time.Duration(index+1)*time.Second)
	}
}

func BenchmarkDialMetricsDisabled(b *testing.B) {
	manager := NewConnectionManager(context.Background(), log.NewNOPFactory().Logger())
	b.ReportAllocs()
	for b.Loop() {
		start := manager.dialMetrics.Start()
		manager.dialMetrics.Finish(start, N.NetworkTCP, "proxy", nil)
	}
}
