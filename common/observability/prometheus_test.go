//go:build with_prometheus

package observability

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
)

func testMetrics(t testing.TB) *Manager {
	t.Helper()
	manager, err := New(option.ObservabilityOptions{
		ListenPort:   9098,
		Metrics:      true,
		MetricsUsers: []string{"allowed"},
	}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	return manager
}

func TestMetricsIsolationAndConnectionLifecycle(t *testing.T) {
	manager := testMetrics(t)
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	conn := manager.Observer.TrackConnection("route", "", left, adapter.InboundContext{Network: N.NetworkTCP, Inbound: "in", User: "excluded"})
	require.NoError(t, N.ReportConnHandshakeSuccess(conn, right))
	var group sync.WaitGroup
	group.Go(func() { conn.Write([]byte("abc")) })
	data := make([]byte, 3)
	_, err := io.ReadFull(right, data)
	require.NoError(t, err)
	group.Wait()
	require.NoError(t, conn.Close())
	conn.Close()
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	require.NoError(t, err)
	for _, metric := range families["sing_box_connections"].Metric {
		require.Zero(t, metric.GetGauge().GetValue())
	}
	for _, metric := range families["sing_box_connections_closed_total"].Metric {
		require.EqualValues(t, 1, metric.GetCounter().GetValue())
	}
	require.Contains(t, w.Body.String(), "go_goroutines")
	require.NotContains(t, w.Body.String(), "excluded")
	other := testMetrics(t)
	w = httptest.NewRecorder()
	other.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.NotContains(t, w.Body.String(), "sing_box_connections{")
}

func TestFlowAndDNSMetrics(t *testing.T) {
	manager := testMetrics(t)
	flow := manager.Observer.TrackFlow("route", "", adapter.InboundContext{Network: N.NetworkUDP, User: "allowed"})
	flow.AttachFlow(nil)
	flow.FlowEstablished()
	flow.CountForward(123)
	flow.CountReverse(45)
	flow.CloseFlow(0)
	flow.CloseFlow(0)
	manager.Observer.ObserveDNS("exchange", "", 1, 3, time.Millisecond, nil)
	manager.Observer.CountDNSCache("dns", "hit")
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `direction="upload",kind="user",network="udp",scope="route",tag="allowed"} 123`)
	require.Contains(t, w.Body.String(), `sing_box_dns_requests_total{kind="exchange",query_type="1",result="3",server=""} 1`)
	require.Contains(t, w.Body.String(), `sing_box_dns_cache_total{result="hit",server="dns"} 1`)
}

func TestMetricResultClassification(t *testing.T) {
	for _, test := range []struct {
		err    error
		result string
	}{
		{nil, "success"},
		{E.Cause(context.Canceled, "wrapped"), "canceled"},
		{E.Cause(context.DeadlineExceeded, "wrapped"), "timeout"},
		{net.ErrClosed, "canceled"},
		{io.EOF, "error"},
	} {
		require.Equal(t, test.result, resultLabel(test.err))
	}
}

type flowTestOutbound struct{ outbound.Adapter }

func (*flowTestOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, E.New("unused dialer")
}

func (*flowTestOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, E.New("unused dialer")
}

func TestNativeFlowKeepsLogicalOutbound(t *testing.T) {
	manager := testMetrics(t)
	group := &flowTestOutbound{Adapter: outbound.NewAdapter(C.TypeSelector, "proxy-group", []string{N.NetworkTCP}, nil)}
	node := &flowTestOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "node", []string{N.NetworkTCP}, nil)}
	flow := manager.Observer.RoutedFlow(context.Background(), adapter.InboundContext{
		Network:       N.NetworkTCP,
		OutboundChain: []adapter.Outbound{group, node},
	}, nil, node)
	flow.AttachFlow(nil)
	flow.FlowEstablished()
	flow.CountForward(4)
	flow.CloseFlow(0)
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `sing_box_flow_bytes_total{direction="upload",kind="outbound",network="tcp",scope="route",tag="proxy-group"} 4`)
	require.Contains(t, w.Body.String(), `sing_box_flow_bytes_total{direction="upload",kind="node",network="tcp",scope="route",tag="node"} 4`)
	require.Contains(t, w.Body.String(), `sing_box_flow_establishment_total{network="tcp",outbound="proxy-group",result="success"} 1`)
	require.NotContains(t, w.Body.String(), "sing_box_traffic_bytes_total{")
}

func TestNativeFlowEstablishmentTCPOnly(t *testing.T) {
	for _, test := range []struct {
		name        string
		network     string
		established bool
		reason      tun.FlowCloseReason
		result      string
	}{
		{"tcp established", N.NetworkTCP, true, tun.FlowCloseFinished, "success"},
		{"tcp early close", N.NetworkTCP, false, tun.FlowCloseTimeout, "error"},
		{"udp timeout", N.NetworkUDP, false, tun.FlowCloseTimeout, ""},
		{"udp reset", N.NetworkUDP, false, tun.FlowCloseReset, ""},
		{"udp finished", N.NetworkUDP, false, tun.FlowCloseFinished, ""},
		{"udp establishment callback", N.NetworkUDP, true, tun.FlowCloseFinished, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := testMetrics(t)
			flow := manager.Observer.TrackFlow(C.MetricScopeRoute, "", adapter.InboundContext{Network: test.network})
			flow.AttachFlow(nil)
			if test.established {
				flow.FlowEstablished()
				flow.FlowEstablished()
			}
			flow.CountForward(123)
			flow.CountReverse(45)
			flow.CloseFlow(test.reason)
			flow.CloseFlow(test.reason)
			w := httptest.NewRecorder()
			manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			parser := expfmt.NewTextParser(model.UTF8Validation)
			families, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
			require.NoError(t, err)
			if test.result == "" {
				require.NotContains(t, families, C.MetricNameFlowEstablishmentTotal)
				require.NotContains(t, families, C.MetricNameFlowEstablishmentDuration)
			} else {
				results := families[C.MetricNameFlowEstablishmentTotal].GetMetric()
				require.Len(t, results, 1)
				require.EqualValues(t, 1, results[0].GetCounter().GetValue())
				require.Contains(t, w.Body.String(), `sing_box_flow_establishment_total{network="tcp",outbound="",result="`+test.result+`"} 1`)
				durations := families[C.MetricNameFlowEstablishmentDuration].GetMetric()
				require.Len(t, durations, 1)
				require.EqualValues(t, 1, durations[0].GetHistogram().GetSampleCount())
			}
			require.Contains(t, w.Body.String(), `sing_box_connections{kind="all",network="`+test.network+`",scope="route",tag=""} 0`)
			require.Contains(t, w.Body.String(), `sing_box_connections_opened_total{kind="all",network="`+test.network+`",scope="route",tag=""} 1`)
			require.Contains(t, w.Body.String(), `sing_box_connections_closed_total{kind="all",network="`+test.network+`",scope="route",tag=""} 1`)
			require.EqualValues(t, 1, families[C.MetricNameConnectionDuration].GetMetric()[0].GetHistogram().GetSampleCount())
			for _, direction := range []string{C.MetricDirectionUpload, C.MetricDirectionDownload} {
				labels := `{direction="` + direction + `",kind="all",network="` + test.network + `",scope="route",tag=""}`
				require.Contains(t, w.Body.String(), C.MetricNameFlowPacketsTotal+labels+` 1`)
				bytes := 123
				if direction == C.MetricDirectionDownload {
					bytes = 45
				}
				require.Contains(t, w.Body.String(), C.MetricNameFlowBytesTotal+labels+" "+strconv.Itoa(bytes))
			}
		})
	}
}

func BenchmarkFlowCounters(b *testing.B) {
	manager := testMetrics(b)
	flow := manager.Observer.TrackFlow("route", "", adapter.InboundContext{Network: N.NetworkTCP})
	flow.AttachFlow(nil)
	b.ReportAllocs()
	for b.Loop() {
		flow.CountForward(1024)
	}
}

func BenchmarkMetricsScrape(b *testing.B) {
	for _, connections := range []int{1, 10000} {
		b.Run(strconv.Itoa(connections), func(b *testing.B) {
			manager := testMetrics(b)
			for range connections {
				flow := manager.Observer.TrackFlow("route", "", adapter.InboundContext{Network: N.NetworkTCP})
				flow.AttachFlow(nil)
				flow.FlowEstablished()
				b.Cleanup(func() { flow.CloseFlow(0) })
			}
			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if w.Code != http.StatusOK {
					b.Fatal(w.Body.String())
				}
			}
		})
	}
}

func TestConcurrentScrapingAndUpdates(t *testing.T) {
	manager := testMetrics(t)
	flow := manager.Observer.TrackFlow("route", "", adapter.InboundContext{Network: N.NetworkTCP})
	flow.AttachFlow(nil)
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 1000 {
			flow.CountForward(1024)
			manager.Observer.ObserveDNS("upstream", "dns", 1, 0, time.Millisecond, nil)
		}
	})
	for range 2 {
		workers.Go(func() {
			for range 10 {
				w := httptest.NewRecorder()
				manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if w.Code != http.StatusOK {
					t.Errorf("scrape failed: %s", w.Body.String())
				}
			}
		})
	}
	workers.Wait()
	flow.CloseFlow(0)
}

func TestUDPPacketCounters(t *testing.T) {
	manager := testMetrics(t)
	left, err := net.ListenPacket(N.NetworkUDP, "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { left.Close() })
	right, err := net.ListenPacket(N.NetworkUDP, "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { right.Close() })
	require.NoError(t, left.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, right.SetDeadline(time.Now().Add(time.Second)))
	tracked := manager.Observer.TrackPacketConnection("route", "", bufio.NewPacketConn(left), adapter.InboundContext{Network: N.NetworkUDP})
	_, err = right.WriteTo([]byte("abc"), left.LocalAddr())
	require.NoError(t, err)
	buffer := buf.NewPacket()
	defer buffer.Release()
	address, err := tracked.ReadPacket(buffer)
	require.NoError(t, err)
	require.Equal(t, "abc", string(buffer.Bytes()))
	require.NoError(t, tracked.WritePacket(buf.As([]byte("def")), address))
	_, _, err = right.ReadFrom(make([]byte, 3))
	require.NoError(t, err)
	require.NoError(t, tracked.Close())
	tracked.Close()
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, direction := range []string{"upload", "download"} {
		require.Contains(t, w.Body.String(), `sing_box_traffic_packets_total{direction="`+direction+`",kind="all",network="udp",scope="route",tag=""} 1`)
	}
}
