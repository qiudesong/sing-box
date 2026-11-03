package route

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func changingRetryController() *acquisitionTestService {
	winner := "proxy"
	return &acquisitionTestService{
		match: func(outbound string) bool { return outbound == winner },
		acquire: func(outbound string) (func(), bool) {
			if outbound == "proxy" {
				winner = "jp"
				return nil, false
			}
			return func() {}, true
		},
	}
}

func TestBGPSelectionRetryRestoresOriginalDestination(t *testing.T) {
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		t.Run(network, func(t *testing.T) {
			r := acquisitionRouter(t, changingRetryController(), `[
				{"ip_cidr":"8.8.4.4/32","outbound":"direct"},
				{"ip_cidr":"8.8.8.8/32","action":"route-options","override_address":"8.8.4.4","override_port":8443},
				{"bgp_bypass":"bypass","outbound":"jp"},
				{"bgp_bypass":"bypass","outbound":"proxy"}
			]`)
			var selected adapter.InboundContext
			for _, outbound := range r.outbound.(*acquisitionTestOutbounds).values {
				outbound.handle = func(conn io.Closer, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
					selected = metadata
					_ = conn.Close()
					if onClose != nil {
						onClose(nil)
					}
				}
			}
			require.NoError(t, routeAcquisitionConnection(t, r, network))
			require.Equal(t, "jp", selected.RouteOutbound)
			require.Equal(t, M.ParseSocksaddr("8.8.4.4:8443"), selected.Destination)
			require.Equal(t, acquisitionMetadata(network).Destination, selected.RouteOriginalDestination)
		})
	}
}

type retryStreamConn struct {
	net.Conn
	payload []byte
	reads   int
}

func (c *retryStreamConn) Read(p []byte) (int, error) {
	c.reads++
	if c.reads != 1 {
		return 0, E.New("unexpected second sniff read")
	}
	return copy(p, c.payload), nil
}

func (*retryStreamConn) SetReadDeadline(time.Time) error { return nil }
func (*retryStreamConn) Close() error                    { return nil }

type retryPacketConn struct {
	N.PacketConn
	payload []byte
	reads   int
}

func (c *retryPacketConn) ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error) {
	c.reads++
	if c.reads != 1 {
		return M.Socksaddr{}, E.New("unexpected second sniff read")
	}
	_, err := buffer.Write(c.payload)
	return acquisitionMetadata(N.NetworkUDP).Destination, err
}

func (*retryPacketConn) SetReadDeadline(time.Time) error { return nil }
func (*retryPacketConn) Close() error                    { return nil }

func TestBGPSelectionRetryReplaysSniffAtOriginalPosition(t *testing.T) {
	for _, network := range []string{N.NetworkTCP, N.NetworkUDP} {
		t.Run(network, func(t *testing.T) {
			protocol := C.ProtocolHTTP
			if network == N.NetworkUDP {
				protocol = C.ProtocolDNS
			}
			r := acquisitionRouter(t, changingRetryController(), `[
				{"protocol":["http","dns"],"outbound":"direct"},
				{"inbound":"lan","action":"sniff","sniffer":["`+protocol+`"]},
				{"bgp_bypass":"bypass","outbound":"jp"},
				{"bgp_bypass":"bypass","outbound":"proxy"}
			]`)
			payload := []byte("GET / HTTP/1.1\r\nHost: example.org\r\n\r\n")
			if network == N.NetworkUDP {
				message := new(dns.Msg)
				message.SetQuestion("example.org.", dns.TypeA)
				var err error
				payload, err = message.Pack()
				require.NoError(t, err)
			}
			var selected adapter.InboundContext
			var received []byte
			for _, outbound := range r.outbound.(*acquisitionTestOutbounds).values {
				outbound.handle = func(conn io.Closer, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
					selected = metadata
					if network == N.NetworkTCP {
						received = make([]byte, len(payload))
						_, err := io.ReadFull(conn.(net.Conn), received)
						require.NoError(t, err)
					} else {
						packet := buf.NewPacket()
						defer packet.Release()
						_, err := conn.(N.PacketConn).ReadPacket(packet)
						require.NoError(t, err)
						received = bytes.Clone(packet.Bytes())
					}
					_ = conn.Close()
					if onClose != nil {
						onClose(nil)
					}
				}
			}
			metadata := acquisitionMetadata(network)
			if network == N.NetworkTCP {
				conn := &retryStreamConn{payload: payload}
				require.NoError(t, r.routeConnection(context.Background(), conn, metadata, nil))
				require.Equal(t, 1, conn.reads)
				require.Equal(t, "example.org", selected.Domain)
			} else {
				conn := &retryPacketConn{payload: payload}
				require.NoError(t, r.routePacketConnection(context.Background(), conn, metadata, nil))
				require.Equal(t, 1, conn.reads)
			}
			require.Equal(t, "jp", selected.RouteOutbound, "earlier rules must not see later sniff results")
			require.Equal(t, protocol, selected.Protocol)
			require.Equal(t, payload, received)
		})
	}
}
