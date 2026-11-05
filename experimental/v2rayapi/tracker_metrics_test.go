package v2rayapi

import (
	"context"
	"net"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-tun"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

type testStatsOutbound struct{ outbound.Adapter }

func (*testStatsOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	panic("unused")
}

func (*testStatsOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	panic("unused")
}

func TestStatsTrackerWithoutMetrics(t *testing.T) {
	server, err := NewServer(log.NewNOPFactory().Logger(), option.V2RayAPIOptions{
		Stats: &option.V2RayStatsServiceOptions{Enabled: true, Users: []string{"user"}},
	})
	require.NoError(t, err)
	scope := adapter.NewScope(context.Background(), log.NewNOPFactory().Logger())
	t.Cleanup(func() { require.NoError(t, scope.Close()) })
	require.NoError(t, server.Start(adapter.StartStateInitialize, scope))
	tracker := server.StatsService()
	stats, supportsStats := tracker.(StatsServiceServer)
	require.True(t, supportsStats)
	outbound := &testStatsOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "out", []string{N.NetworkTCP}, nil)}
	flow := tracker.RoutedFlow(context.Background(), adapter.InboundContext{User: "user"}, nil, outbound)
	require.NotNil(t, flow)
	flow.AttachFlow(nil)
	flow.FlowEstablished()
	flow.CountForward(40)
	flow.CountReverse(60)
	flow.CloseFlow(tun.FlowCloseFinished)
	response, err := stats.GetStats(context.Background(), &GetStatsRequest{Name: "user>>>user>>>traffic>>>uplink"})
	require.NoError(t, err)
	require.EqualValues(t, 40, response.Stat.Value)
	response, err = stats.GetStats(context.Background(), &GetStatsRequest{Name: "user>>>user>>>traffic>>>downlink"})
	require.NoError(t, err)
	require.EqualValues(t, 60, response.Stat.Value)
}

func TestDisabledStatsTracker(t *testing.T) {
	server, err := NewServer(log.NewNOPFactory().Logger(), option.V2RayAPIOptions{})
	require.NoError(t, err)
	scope := adapter.NewScope(context.Background(), log.NewNOPFactory().Logger())
	t.Cleanup(func() { require.NoError(t, scope.Close()) })
	require.NoError(t, server.Start(adapter.StartStateInitialize, scope))
	require.Nil(t, server.StatsService())
}
