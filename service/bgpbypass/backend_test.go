package bgpbypass

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/log"

	"github.com/osrg/gobgp/v4/api"
	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
	"github.com/osrg/gobgp/v4/pkg/server"
	"github.com/stretchr/testify/require"
)

func TestNativeBGPIPv4AndIPv6RIB(t *testing.T) {
	c := testConfig(t)
	o := c.options.BGP
	o.Peers = nil
	b := newBGPBackend(o, log.NewNOPFactory().Logger())
	require.NoError(t, b.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	prefixes := []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32"), netip.MustParsePrefix("2606:4700:4700::1111/128")}
	require.NoError(t, b.Add(prefixes))
	actual, err := b.List()
	require.NoError(t, err)
	require.Len(t, actual, 2)
	for _, p := range prefixes {
		require.True(t, actual[p])
	}
	require.NoError(t, b.Delete(prefixes))
	actual, err = b.List()
	require.NoError(t, err)
	require.Empty(t, actual)
}

// Exercise actual BGP UPDATE exchange over an unprivileged loopback session.
// Local RIB tests alone cannot detect export filtering or rejected next hops.
func TestNativeBGPSessionExportAndImportIsolation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	ctx := context.Background()
	remote := server.NewBgpServer()
	go remote.Serve()
	t.Cleanup(remote.Stop)
	require.NoError(t, remote.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{
		Asn:             65001,
		RouterId:        "127.0.0.2",
		ListenPort:      int32(port),
		ListenAddresses: []string{"127.0.0.1"},
	}}))
	families := []*api.AfiSafi{
		{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}, Enabled: true}},
		{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}, Enabled: true}},
	}
	require.NoError(t, remote.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65002},
		Transport: &api.Transport{PassiveMode: true},
		AfiSafis:  families,
	}}))
	o := testConfig(t).options.BGP
	o.Peers = nil
	b := newBGPBackend(o, log.NewNOPFactory().Logger())
	require.NoError(t, b.Start(ctx))
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.server.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
		Conf:      &api.PeerConf{NeighborAddress: "127.0.0.1", PeerAsn: 65001},
		Transport: &api.Transport{LocalAddress: "127.0.0.1", RemotePort: uint32(port)},
		AfiSafis:  families,
	}}))
	prefixes := []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32"), netip.MustParsePrefix("2606:4700:4700::1111/128")}
	require.NoError(t, b.Add(prefixes))
	contains := func(s *server.BgpServer, family bgp.Family, prefix string) bool {
		found := false
		err := s.ListPath(apiutil.ListPathRequest{TableType: api.TableType_TABLE_TYPE_GLOBAL, Family: family}, func(nlri bgp.NLRI, paths []*apiutil.Path) {
			if nlri.String() == prefix && len(paths) > 0 {
				found = true
			}
		})
		require.NoError(t, err)
		return found
	}
	require.Eventually(t, func() bool {
		return contains(remote, bgp.RF_IPv4_UC, prefixes[0].String()) && contains(remote, bgp.RF_IPv6_UC, prefixes[1].String())
	}, 15*time.Second, 20*time.Millisecond)
	foreign, err := b.path(netip.MustParsePrefix("9.9.9.9/32"))
	require.NoError(t, err)
	_, err = remote.AddPath(apiutil.AddPathRequest{Paths: []*apiutil.Path{foreign}})
	require.NoError(t, err)
	// Prove the foreign UPDATE arrived in Adj-RIB-In before checking rejection.
	require.Eventually(t, func() bool {
		found := false
		err := b.server.ListPath(apiutil.ListPathRequest{
			TableType: api.TableType_TABLE_TYPE_ADJ_IN,
			Name:      "127.0.0.1",
			Family:    bgp.RF_IPv4_UC,
		}, func(nlri bgp.NLRI, _ []*apiutil.Path) { found = found || nlri.String() == "9.9.9.9/32" })
		require.NoError(t, err)
		return found
	}, 5*time.Second, 20*time.Millisecond)
	require.False(t, contains(b.server, bgp.RF_IPv4_UC, "9.9.9.9/32"))
	require.NoError(t, b.Delete(prefixes))
	require.Eventually(t, func() bool {
		return !contains(remote, bgp.RF_IPv4_UC, prefixes[0].String()) && !contains(remote, bgp.RF_IPv6_UC, prefixes[1].String())
	}, 5*time.Second, 20*time.Millisecond)
}
