//go:build with_prometheus

package box_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

func TestObservabilityBoxRestartAndIsolation(t *testing.T) {
	listener, err := net.Listen(N.NetworkTCP, "127.0.0.1:0")
	require.NoError(t, err)
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())
	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)
	for range 2 {
		instance, err := box.New(box.Options{
			Context: include.Context(context.Background()),
			Options: option.Options{
				Observability: &option.ObservabilityOptions{ListenPort: port, Metrics: true},
				Outbounds:     []option.Outbound{{Type: C.TypeDirect, Tag: "direct", Options: &option.DirectOutboundOptions{}}},
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { instance.Close() })
		require.NoError(t, instance.Start())
		for _, path := range []string{"/readyz", "/metrics"} {
			response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(int(port)) + path)
			require.NoError(t, err)
			content, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			require.NoError(t, readErr)
			require.Equal(t, http.StatusOK, response.StatusCode, string(content))
			if path == "/metrics" {
				require.True(t, strings.Contains(string(content), "go_goroutines"))
			}
		}
		client.CloseIdleConnections()
		require.NoError(t, instance.Close())
	}
}
