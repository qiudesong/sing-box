package route

import (
	"io"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	N "github.com/sagernet/sing/common/network"

	"github.com/stretchr/testify/require"
)

type resolutionLeaf struct {
	outbound.Adapter
	N.Dialer
}
type resolutionGroup struct {
	adapter.Outbound
	next adapter.Outbound
}

func (g *resolutionGroup) All() []string                     { return []string{g.next.Tag()} }
func (g *resolutionGroup) Selected(string) adapter.Outbound  { return g.next }
func (g *resolutionGroup) AttachConnection(io.Closer) func() { return func() {} }

func TestNativeOutboundPreservesGroupChain(t *testing.T) {
	leaf := &resolutionLeaf{Adapter: outbound.NewAdapter("test", "leaf", []string{N.NetworkTCP, N.NetworkUDP}, nil)}
	group := &resolutionGroup{Outbound: leaf, next: leaf}
	chain, err := resolveOutbound(group, N.NetworkUDP)
	require.NoError(t, err)
	require.Equal(t, []adapter.Outbound{group, leaf}, chain)
	// Groups continue to choose nodes per connection.
	other := &resolutionLeaf{Adapter: outbound.NewAdapter("test", "other", []string{N.NetworkTCP}, nil)}
	group.next = other
	chain, err = resolveOutbound(group, N.NetworkTCP)
	require.NoError(t, err)
	require.Same(t, other, chain[len(chain)-1])
	_, err = resolveOutbound(group, N.NetworkUDP)
	require.Error(t, err)
}

func TestNativeOutboundCyclesAndUnavailable(t *testing.T) {
	leaf := &resolutionLeaf{Adapter: outbound.NewAdapter("test", "leaf", []string{N.NetworkTCP}, nil)}
	group := &resolutionGroup{Outbound: leaf}
	group.next = group
	_, err := resolveOutbound(group, N.NetworkTCP)
	require.ErrorContains(t, err, "cycle")
	group.next = nil
	_, err = resolveOutbound(group, N.NetworkTCP)
	require.ErrorContains(t, err, "not supported")
}
