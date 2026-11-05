//go:build with_prometheus

package v2rayapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/observability"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

func TestPacketMetricsSurviveStatsReset(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	ctx := adapter.ContextWithMetricsObserver(context.Background(), manager.Observer)
	stats := NewStatsService(option.V2RayStatsServiceOptions{Enabled: true, Outbounds: []string{"out"}})
	tracker := (&Server{statsService: stats}).StatsService()
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	require.NoError(t, left.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, right.SetDeadline(time.Now().Add(time.Second)))
	outbound := &testStatsOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "out", []string{N.NetworkUDP}, nil)}
	conn := tracker.RoutedPacketConnection(ctx, bufio.NewUnbindPacketConn(left), adapter.InboundContext{Network: N.NetworkUDP}, nil, outbound)
	var workers sync.WaitGroup
	var writeErr error
	workers.Go(func() { writeErr = conn.WritePacket(buf.As([]byte("abc")), M.Socksaddr{}) })
	_, err = io.ReadFull(right, make([]byte, 3))
	require.NoError(t, err)
	workers.Wait()
	require.NoError(t, writeErr)
	response, err := stats.GetStats(ctx, &GetStatsRequest{Name: "outbound>>>out>>>traffic>>>downlink", Reset_: true})
	require.NoError(t, err)
	require.EqualValues(t, 3, response.Stat.Value)
	response, err = stats.GetStats(ctx, &GetStatsRequest{Name: "outbound>>>out>>>traffic>>>downlink"})
	require.NoError(t, err)
	require.Zero(t, response.Stat.Value)
	require.NoError(t, conn.Close())
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `sing_box_traffic_bytes_total{direction="download",kind="outbound",network="udp",scope="v2ray",tag="out"} 3`)
	require.Contains(t, w.Body.String(), `sing_box_traffic_packets_total{direction="download",kind="outbound",network="udp",scope="v2ray",tag="out"} 1`)
}

func TestFlowMetricsSurviveStatsReset(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	ctx := adapter.ContextWithMetricsObserver(context.Background(), manager.Observer)
	stats := NewStatsService(option.V2RayStatsServiceOptions{Enabled: true, Inbounds: []string{"in"}})
	tracker := (&Server{statsService: stats}).StatsService()
	outbound := &testStatsOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "out", []string{N.NetworkTCP}, nil)}
	flow := tracker.RoutedFlow(ctx, adapter.InboundContext{
		Inbound:       "in",
		Network:       N.NetworkTCP,
		User:          "private",
		RouteOutbound: "hidden",
		OutboundChain: []adapter.Outbound{outbound},
	}, nil, outbound)
	require.NotNil(t, flow)
	flow.AttachFlow(nil)
	flow.CountForward(40)
	flow.CountReverse(60)
	flow.FlowEstablished()
	response, err := stats.GetStats(ctx, &GetStatsRequest{Name: "inbound>>>in>>>traffic>>>uplink", Reset_: true})
	require.NoError(t, err)
	require.EqualValues(t, 40, response.Stat.Value)
	response, err = stats.GetStats(ctx, &GetStatsRequest{Name: "inbound>>>in>>>traffic>>>downlink", Reset_: true})
	require.NoError(t, err)
	require.EqualValues(t, 60, response.Stat.Value)
	flow.CountForward(10)
	response, err = stats.GetStats(ctx, &GetStatsRequest{Name: "inbound>>>in>>>traffic>>>uplink"})
	require.NoError(t, err)
	require.EqualValues(t, 10, response.Stat.Value)
	flow.CloseFlow(tun.FlowCloseFinished)
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `sing_box_flow_bytes_total{direction="upload",kind="inbound",network="tcp",scope="v2ray",tag="in"} 50`)
	require.Contains(t, w.Body.String(), `sing_box_flow_bytes_total{direction="download",kind="inbound",network="tcp",scope="v2ray",tag="in"} 60`)
	require.Contains(t, w.Body.String(), `sing_box_connections{kind="inbound",network="tcp",scope="v2ray",tag="in"} 0`)
	require.Contains(t, w.Body.String(), `sing_box_connections_closed_total{kind="inbound",network="tcp",scope="v2ray",tag="in"} 1`)
	require.NotContains(t, w.Body.String(), "private")
	require.NotContains(t, w.Body.String(), `scope="v2ray",tag="out"`)
	require.NotContains(t, w.Body.String(), "hidden")
}

func TestMetricsIgnoreUnselectedTraffic(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	ctx := adapter.ContextWithMetricsObserver(context.Background(), manager.Observer)
	stats := NewStatsService(option.V2RayStatsServiceOptions{
		Enabled:   true,
		Inbounds:  []string{"selected", ""},
		Outbounds: []string{"selected", ""},
		Users:     []string{"selected", ""},
	})
	tracker := (&Server{statsService: stats}).StatsService()
	outbound := &testStatsOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "out", []string{N.NetworkTCP, N.NetworkUDP}, nil)}
	for _, metadata := range []adapter.InboundContext{{}, {Inbound: "other", User: "private"}} {
		require.Nil(t, tracker.RoutedConnection(ctx, nil, metadata, nil, outbound))
		require.Nil(t, tracker.RoutedPacketConnection(ctx, nil, metadata, nil, outbound))
		require.Nil(t, tracker.RoutedFlow(ctx, metadata, nil, outbound))
	}
	response, err := stats.QueryStats(ctx, &QueryStatsRequest{})
	require.NoError(t, err)
	require.Empty(t, response.Stat)
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, w.Body.String(), `scope="v2ray"`)
}

func TestMetricsSurviveStatsReset(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	ctx := adapter.ContextWithMetricsObserver(context.Background(), manager.Observer)
	stats := NewStatsService(option.V2RayStatsServiceOptions{Enabled: true, Inbounds: []string{"in"}})
	tracker := (&Server{statsService: stats}).StatsService()
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	outbound := &testStatsOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "out", []string{N.NetworkTCP}, nil)}
	conn := tracker.RoutedConnection(ctx, left, adapter.InboundContext{Inbound: "in", Network: N.NetworkTCP}, nil, outbound)
	var workers sync.WaitGroup
	workers.Go(func() { conn.Write([]byte("abc")) })
	_, err = io.ReadFull(right, make([]byte, 3))
	require.NoError(t, err)
	workers.Wait()
	response, err := stats.GetStats(ctx, &GetStatsRequest{Name: "inbound>>>in>>>traffic>>>downlink", Reset_: true})
	require.NoError(t, err)
	require.EqualValues(t, 3, response.Stat.Value)
	response, err = stats.GetStats(ctx, &GetStatsRequest{Name: "inbound>>>in>>>traffic>>>downlink"})
	require.NoError(t, err)
	require.Zero(t, response.Stat.Value)
	conn.Close()
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `sing_box_traffic_bytes_total{direction="download",kind="inbound",network="tcp",scope="v2ray",tag="in"} 3`)
}
