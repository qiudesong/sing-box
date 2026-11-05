//go:build with_prometheus

package ssmapi

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/observability"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

func TestMetricsSurviveAPIClear(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	traffic := NewTrafficManager()
	tracker := newMetricsTracker(traffic, manager.Observer, "ssm:in")
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	conn := tracker.TrackConnection(left, adapter.InboundContext{Inbound: "in", User: "private", Network: N.NetworkTCP})
	var workers sync.WaitGroup
	workers.Go(func() { conn.Write([]byte("abc")) })
	_, err = io.ReadFull(right, make([]byte, 3))
	require.NoError(t, err)
	workers.Wait()
	_, down, _, _, tcp, _ := traffic.ReadGlobal(true)
	require.EqualValues(t, 3, down)
	require.EqualValues(t, 1, tcp)
	_, down, _, _, tcp, _ = traffic.ReadGlobal(false)
	require.Zero(t, down)
	require.Zero(t, tcp)
	conn.Close()
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `sing_box_traffic_bytes_total{direction="download",kind="all",network="tcp",scope="ssm",tag="ssm:in"} 3`)
	require.NotContains(t, w.Body.String(), "private")
}

func TestPacketMetricsSurviveAPIClear(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	traffic := NewTrafficManager()
	tracker := newMetricsTracker(traffic, manager.Observer, "ssm:in")
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	require.NoError(t, left.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, right.SetDeadline(time.Now().Add(time.Second)))
	conn := tracker.TrackPacketConnection(bufio.NewUnbindPacketConn(left), adapter.InboundContext{User: "private"})
	var workers sync.WaitGroup
	var writeErr error
	workers.Go(func() { _, writeErr = right.Write([]byte("abc")) })
	buffer := buf.NewPacket()
	defer buffer.Release()
	_, err = conn.ReadPacket(buffer)
	require.NoError(t, err)
	workers.Wait()
	require.NoError(t, writeErr)
	require.Equal(t, "abc", string(buffer.Bytes()))
	workers.Go(func() { writeErr = conn.WritePacket(buf.As([]byte("def")), M.Socksaddr{}) })
	_, err = io.ReadFull(right, make([]byte, 3))
	require.NoError(t, err)
	workers.Wait()
	require.NoError(t, writeErr)
	up, down, upPackets, downPackets, _, udp := traffic.ReadGlobal(true)
	require.EqualValues(t, 3, up)
	require.EqualValues(t, 3, down)
	require.EqualValues(t, 1, upPackets)
	require.EqualValues(t, 1, downPackets)
	require.EqualValues(t, 1, udp)
	up, down, upPackets, downPackets, _, udp = traffic.ReadGlobal(false)
	require.Zero(t, up)
	require.Zero(t, down)
	require.Zero(t, upPackets)
	require.Zero(t, downPackets)
	require.Zero(t, udp)
	require.NoError(t, conn.Close())
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	for _, direction := range []string{"upload", "download"} {
		require.Contains(t, w.Body.String(), `sing_box_traffic_bytes_total{direction="`+direction+`",kind="all",network="udp",scope="ssm",tag="ssm:in"} 3`)
		require.Contains(t, w.Body.String(), `sing_box_traffic_packets_total{direction="`+direction+`",kind="all",network="udp",scope="ssm",tag="ssm:in"} 1`)
	}
	require.NotContains(t, w.Body.String(), "private")
}
