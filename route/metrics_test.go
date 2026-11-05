//go:build with_prometheus

package route

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/observability"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

type metricsFailDialer struct{}

func (metricsFailDialer) DialContext(context.Context, string, M.Socksaddr) (net.Conn, error) {
	return nil, context.Canceled
}

func (metricsFailDialer) ListenPacket(context.Context, M.Socksaddr) (net.PacketConn, error) {
	return nil, context.Canceled
}

func TestDialMetricsPreserveFailure(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		network    string
		udpConnect bool
	}{
		{name: "tcp", network: N.NetworkTCP},
		{name: "udp_connect", network: N.NetworkUDP, udpConnect: true},
		{name: "udp_listen", network: N.NetworkUDP},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
			require.NoError(t, err)
			ctx := adapter.ContextWithMetricsObserver(context.Background(), manager.Observer)
			connections := NewConnectionManager(ctx, log.NewNOPFactory().Logger())
			left, right := net.Pipe()
			t.Cleanup(func() { left.Close(); right.Close() })
			metadata := adapter.InboundContext{
				RouteOutbound: "proxy",
				UDPConnect:    testCase.udpConnect,
			}
			var failure error
			onClose := func(err error) { failure = err }
			if testCase.network == N.NetworkTCP {
				connections.NewConnection(ctx, metricsFailDialer{}, left, metadata, onClose)
			} else {
				connections.NewPacketConnection(ctx, metricsFailDialer{}, bufio.NewUnbindPacketConn(left), metadata, onClose)
			}
			require.ErrorIs(t, failure, context.Canceled)
			w := httptest.NewRecorder()
			manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `sing_box_dial_total{network="`+testCase.network+`",outbound="proxy",result="canceled"} 1`)
		})
	}
}
