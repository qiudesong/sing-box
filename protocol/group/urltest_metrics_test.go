//go:build with_prometheus

package group

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/interrupt"
	"github.com/sagernet/sing-box/common/observability"
	"github.com/sagernet/sing-box/common/urltest"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

type metricsTestOutbound struct{ outbound.Adapter }

func (*metricsTestOutbound) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, context.Canceled
}

func (*metricsTestOutbound) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, context.Canceled
}

func TestURLTestSelectedFallback(t *testing.T) {
	tcp := &metricsTestOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "tcp", []string{N.NetworkTCP}, nil)}
	udp := &metricsTestOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "udp", []string{N.NetworkUDP}, nil)}
	group := &URLTest{group: &URLTestGroup{
		outbounds:      []adapter.Outbound{tcp, udp},
		history:        urltest.NewHistoryStorage(),
		interruptGroup: interrupt.NewGroup(),
	}}
	// Before any update, Selected must retain its fallback to a supported node.
	require.Same(t, tcp, group.Selected(N.NetworkTCP))
	require.Same(t, udp, group.Selected(N.NetworkUDP))
	group.PerformUpdateCheck()
	require.Same(t, tcp, group.Selected(N.NetworkTCP))
	require.Same(t, udp, group.Selected(N.NetworkUDP))
	group.group.outbounds = nil
	group.group.selectedOutboundTCP = nil
	group.group.selectedOutboundUDP = nil
	require.Nil(t, group.Selected(N.NetworkTCP))
	require.Nil(t, group.Selected(N.NetworkUDP))
}

func TestURLTestMetricsConcurrentSelection(t *testing.T) {
	history := urltest.NewHistoryStorage()
	first := &metricsTestOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "first", []string{N.NetworkTCP, N.NetworkUDP}, nil)}
	second := &metricsTestOutbound{Adapter: outbound.NewAdapter(C.TypeDirect, "second", []string{N.NetworkTCP, N.NetworkUDP}, nil)}
	group := &URLTest{
		Adapter: outbound.NewAdapter(C.TypeURLTest, "group", nil, nil),
		tags:    []string{first.Tag(), second.Tag()},
		group: &URLTestGroup{
			outbounds:      []adapter.Outbound{first, second},
			history:        history,
			interruptGroup: interrupt.NewGroup(),
		},
	}
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	manager.AddOutbound(group, history)
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() {
		<-start
		for index := range 5000 {
			firstDelay, secondDelay := uint16(1), uint16(100)
			if index%2 != 0 {
				firstDelay, secondDelay = secondDelay, firstDelay
			}
			history.StoreURLTestHistory(first.Tag(), &adapter.URLTestHistory{Time: time.Now(), Delay: firstDelay})
			history.StoreURLTestHistory(second.Tag(), &adapter.URLTestHistory{Time: time.Now(), Delay: secondDelay})
			group.PerformUpdateCheck()
		}
	})
	workers.Go(func() {
		<-start
		for range 100 {
			response := httptest.NewRecorder()
			manager.Handler(false).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if response.Code != http.StatusOK {
				t.Errorf("scrape failed: %s", response.Body.String())
			}
			for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
				selected := group.Selected(network)
				if selected != first && selected != second {
					t.Errorf("unexpected %s selection: %v", network, selected)
				}
			}
		}
	})
	close(start)
	workers.Wait()
}
