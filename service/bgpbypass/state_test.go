package bgpbypass

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/stretchr/testify/require"
)

type unloadedRuleSet struct{ adapter.RuleSet }

func (unloadedRuleSet) Metadata() adapter.RuleSetMetadata { return adapter.RuleSetMetadata{} }

func TestUnloadedRemoteSetDoesNotHideInvalidExplicitPrefix(t *testing.T) {
	c := testConfig(t)
	c.sets["remote"] = unloadedRuleSet{}
	c.options.PersistentRoutes = []option.BGPBypassPersistentRoute{{
		IPCIDR:   []string{"0.0.0.0/0"},
		RuleSet:  []string{"remote"},
		Outbound: "proxy",
	}}
	_, err := c.buildFixed(true)
	require.ErrorContains(t, err, "default route")
	c.options.PersistentRoutes[0].IPCIDR = []string{"8.8.8.8/32"}
	fixed, err := c.buildFixed(true)
	require.NoError(t, err)
	require.Len(t, fixed, 1)
	_, err = c.buildFixed()
	require.ErrorContains(t, err, "pure prefix union")
}

func testConfig(t testing.TB) *compiledConfig {
	t.Helper()
	delay := badoption.Duration(time.Second)
	settle := badoption.Duration(0)
	o := option.BGPBypassServiceOptions{
		DNSInbounds: []string{"lan"},
		BGP: option.BGPBypassBGPOptions{
			LocalASN:    65002,
			RouterID:    "192.168.66.2",
			NextHopIPv4: "192.168.66.2",
			NextHopIPv6: "fd66::2",
			Peers: []option.BGPBypassPeer{{
				Tag:          "v4",
				Address:      "192.168.66.1",
				LocalAddress: "192.168.66.2",
				ASN:          65001,
				Families:     []string{"ipv4-unicast"},
			}, {
				Tag:          "v6",
				Address:      "fd66::1",
				LocalAddress: "fd66::2",
				ASN:          65001,
				Families:     []string{"ipv6-unicast"},
			}},
		},
		Timing: option.BGPBypassTiming{WithdrawDelay: &delay, SettleDelay: &settle},
	}
	require.NoError(t, normalize(&o))
	c, err := compile(context.Background(), nil, o)
	require.NoError(t, err)
	for i, raw := range []option.DefaultRule{
		{RawDefaultRule: option.RawDefaultRule{Domain: []string{"jp.example"}}, RuleAction: option.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: option.RouteActionOptions{Outbound: "jp"}}},
		{RawDefaultRule: option.RawDefaultRule{DomainSuffix: []string{"example"}}, RuleAction: option.RuleAction{Action: C.RuleActionTypeRoute, RouteOptions: option.RouteActionOptions{Outbound: "proxy"}}},
	} {
		matcher, err := R.NewDefaultRule(context.Background(), log.NewNOPFactory().Logger(), raw)
		require.NoError(t, err)
		c.rules = append(c.rules, learnedRule{rule: matcher, decision: decision{Outbound: raw.RouteOptions.Outbound, Priority: i}})
	}
	return c
}

func TestSharedIPLeasesAndConnections(t *testing.T) {
	c := testConfig(t)
	s := newRouteState(c)
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	p := netip.PrefixFrom(ip, 32)
	require.True(t, s.learn(association{
		Domain:  "jp.example",
		Address: ip,
		Expires: now.Add(time.Second),
	}, now))
	require.True(t, s.learn(association{
		Domain:  "other.example",
		Address: ip,
		Expires: now.Add(3 * time.Second),
	}, now))
	require.True(t, s.learn(association{
		Domain:  "direct.test",
		Address: ip,
		Expires: now.Add(5 * time.Second),
	}, now))
	require.Equal(t, "jp", s.lookup(ip, now))
	require.True(t, s.desired(now)[p])
	require.EqualValues(t, 1, s.conflicts.Load())
	release := s.pin(ip, "jp")
	s.expire(now.Add(2 * time.Second))
	require.Equal(t, "proxy", s.lookup(ip, now.Add(2*time.Second)))
	s.expire(now.Add(4 * time.Second))
	require.Empty(t, s.lookup(ip, now.Add(4*time.Second)))
	require.True(t, s.desired(now.Add(4 * time.Second))[p])
	release()
	release()
	require.True(t, s.desired(now.Add(4 * time.Second))[p])
	s.expire(now.Add(6 * time.Second))
	require.False(t, s.desired(now.Add(6 * time.Second))[p])
	require.Zero(t, s.count.Load())
	require.Zero(t, s.addresses.Load())
}

func TestAcquireRetainsRouteUntilRelease(t *testing.T) {
	s := newRouteState(testConfig(t))
	s.withdrawDelay = 0
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	prefix := netip.PrefixFrom(ip, 32)
	require.True(t, s.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now))
	require.Equal(t, "proxy", s.lookup(ip, now))
	require.Empty(t, s.shard(ip).entries[ip].active, "matching must remain read-only")
	release, acquired := s.acquire(ip, "proxy")
	require.True(t, acquired)
	require.NotNil(t, release)
	s.expire(now.Add(2 * time.Hour))
	require.True(t, s.desired(now.Add(2 * time.Hour))[prefix])
	release()
	release()
	require.False(t, s.desired(now.Add(2 * time.Hour))[prefix])
	require.Zero(t, s.routeSlots.Load())
	require.Zero(t, s.addresses.Load())
}

func TestAcquireRejectsStaleSelection(t *testing.T) {
	for _, change := range []string{"expired", "withdrawal grace", "winner changed"} {
		t.Run(change, func(t *testing.T) {
			s := newRouteState(testConfig(t))
			s.withdrawDelay = 0
			if change == "withdrawal grace" {
				s.withdrawDelay = time.Minute
			}
			now := time.Now()
			ip := netip.MustParseAddr("8.8.8.8")
			require.True(t, s.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now))
			require.Equal(t, "proxy", s.lookup(ip, now))
			if change != "winner changed" {
				s.desired(now)
				s.expire(now.Add(2 * time.Hour))
				s.desired(now.Add(2 * time.Hour))
			} else {
				require.True(t, s.learn(association{Domain: "jp.example", Address: ip, Expires: now.Add(time.Hour)}, now))
			}
			release, acquired := s.acquire(ip, "proxy")
			require.False(t, acquired)
			require.Nil(t, release)
			if change == "expired" {
				require.Zero(t, s.addresses.Load(), "acquisition must not recreate an expired route")
			} else {
				require.Empty(t, s.shard(ip).entries[ip].active)
			}
		})
	}
}

func TestAcquirePersistentRouteAndDynamicPrecedence(t *testing.T) {
	s := newRouteState(testConfig(t))
	ip := netip.MustParseAddr("8.8.8.8")
	s.fixed[netip.MustParsePrefix("8.8.8.0/24")] = "jp"
	release, acquired := s.acquire(ip, "jp")
	require.True(t, acquired)
	require.Nil(t, release, "persistent routes need no connection reference")
	now := time.Now()
	require.True(t, s.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now))
	release, acquired = s.acquire(ip, "jp")
	require.False(t, acquired, "the dynamic winner takes precedence over a fixed route")
	require.Nil(t, release)
}

func TestPendingPolicyKeepsFixedFallbackUntilAdmission(t *testing.T) {
	c := testConfig(t)
	c.options.Limits.MaxDynamicRoutes = 1
	s := newRouteState(c)
	s.withdrawDelay = 0
	now := time.Now()
	a := netip.MustParseAddr("8.8.8.8")
	b := netip.MustParseAddr("8.8.4.4")
	s.fixed[netip.PrefixFrom(b, 32)] = "jp"
	require.True(t, s.learn(association{
		Domain:  "a.example",
		Address: a,
		Expires: now.Add(time.Second),
	}, now))
	require.True(t, s.learn(association{
		Domain:  "b.test",
		Address: b,
		Expires: now.Add(time.Hour),
	}, now))
	c.final = decision{Outbound: "proxy", Priority: len(c.rules)}
	s.recompute()
	s.desired(now)
	require.Equal(t, "jp", s.lookup(b, now))
	release, acquired := s.acquire(b, "jp")
	require.True(t, acquired)
	require.Nil(t, release)
	release, acquired = s.acquire(b, "proxy")
	require.False(t, acquired)
	require.Nil(t, release)
	s.expire(now.Add(2 * time.Second))
	s.desired(now.Add(2 * time.Second))
	require.EqualValues(t, 1, s.routeSlots.Load())
	require.Equal(t, "proxy", s.lookup(b, now))
	release, acquired = s.acquire(b, "proxy")
	require.True(t, acquired)
	require.NotNil(t, release)
	release()
}

func TestAcquireConcurrentExpiry(t *testing.T) {
	c := testConfig(t)
	for range 100 {
		s := newRouteState(c)
		s.withdrawDelay = 0
		now := time.Now()
		ip := netip.MustParseAddr("8.8.8.8")
		prefix := netip.PrefixFrom(ip, 32)
		require.True(t, s.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now))
		s.desired(now)
		start := make(chan struct{})
		var release func()
		var acquired bool
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			release, acquired = s.acquire(ip, "proxy")
		})
		wg.Go(func() {
			<-start
			s.expire(now.Add(2 * time.Hour))
			s.desired(now.Add(2 * time.Hour))
		})
		close(start)
		wg.Wait()
		require.Equal(t, acquired, s.desired(now.Add(2 * time.Hour))[prefix])
		if acquired {
			release()
			release()
			require.False(t, s.desired(now.Add(2 * time.Hour))[prefix])
		}
	}
}

func TestRefreshDoesNotGrowExpiryHeap(t *testing.T) {
	s := newRouteState(testConfig(t))
	now := time.Now()
	ip := netip.MustParseAddr("8.8.4.4")
	for i := range 1000 {
		require.True(t, s.learn(association{
			Domain:  "a.example",
			Address: ip,
			Expires: now.Add(time.Duration(i+1) * time.Second),
		}, now))
	}
	require.Len(t, s.shard(ip).expiry, 1)
	s.expire(now.Add(500 * time.Second))
	require.EqualValues(t, 1, s.count.Load())
	s.expire(now.Add(1001 * time.Second))
	require.Zero(t, s.count.Load())
}

func TestCapacityAndExclusions(t *testing.T) {
	c := testConfig(t)
	c.options.Limits.MaxDynamicRoutes = 1
	c.options.Limits.MaxDNSAssociations = 1
	s := newRouteState(c)
	now := time.Now()
	add := func(ip, domain string) bool {
		return s.learn(association{
			Domain:  domain,
			Address: netip.MustParseAddr(ip),
			Expires: now.Add(time.Minute),
		}, now)
	}
	require.False(t, add("127.0.0.1", "a.example"))
	require.False(t, add("fd66::2", "a.example"))
	require.False(t, add("4000::1", "a.example"))
	require.False(t, add("3fff::1", "a.example"))
	require.False(t, add("2001:2::1", "a.example"))
	require.True(t, add("8.8.8.8", "a.example"))
	require.False(t, add("8.8.4.4", "b.example"))
	require.False(t, add("8.8.8.8", "b.example"))
	require.Equal(t, "proxy", s.lookup(netip.MustParseAddr("8.8.8.8"), now))
}

func TestDirectAssociationsDoNotConsumeRouteSlots(t *testing.T) {
	c := testConfig(t)
	c.options.Limits.MaxDynamicRoutes = 1
	s := newRouteState(c)
	now := time.Now()
	for _, text := range []string{"8.8.8.8", "8.8.4.4"} {
		require.True(t, s.learn(association{
			Domain:  "direct.test",
			Address: netip.MustParseAddr(text),
			Expires: now.Add(time.Hour),
		}, now))
	}
	require.Zero(t, s.routeSlots.Load())
	ip := netip.MustParseAddr("8.8.8.8")
	require.True(t, s.learn(association{
		Domain:  "a.example",
		Address: ip,
		Expires: now.Add(time.Second),
	}, now))
	require.False(t, s.learn(association{
		Domain:  "b.example",
		Address: netip.MustParseAddr("8.8.4.4"),
		Expires: now.Add(time.Hour),
	}, now))
	require.True(t, s.desired(now)[netip.PrefixFrom(ip, 32)])
	s.expire(now.Add(2 * time.Second))
	s.desired(now.Add(2 * time.Second))
	s.desired(now.Add(4 * time.Second))
	require.Zero(t, s.routeSlots.Load())
	require.EqualValues(t, 2, s.count.Load())
}

func TestFixedLongestPrefixAndDynamicPriority(t *testing.T) {
	s := newRouteState(testConfig(t))
	ip := netip.MustParseAddr("8.8.8.8")
	s.fixed[netip.MustParsePrefix("8.8.0.0/16")] = "broad"
	s.fixed[netip.MustParsePrefix("8.8.8.0/24")] = "narrow"
	now := time.Now()
	require.Equal(t, "narrow", s.lookup(ip, now))
	s.learn(association{
		Domain:  "jp.example",
		Address: ip,
		Expires: now.Add(time.Second),
	}, now)
	require.Equal(t, "jp", s.lookup(ip, now))
	require.Equal(t, "narrow", s.lookup(ip, now.Add(2*time.Second)))
}

func TestConcurrentLookupAndClose(t *testing.T) {
	s := newRouteState(testConfig(t))
	now := time.Now()
	ip := netip.MustParseAddr("2606:4700:4700::1111")
	s.learn(association{
		Domain:  "a.example",
		Address: ip,
		Expires: now.Add(time.Hour),
	}, now)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				_ = s.lookup(ip, time.Now())
				release := s.pin(ip, "proxy")
				release()
				release()
			}
		})
	}
	for range 100 {
		s.recompute()
		s.desired(time.Now())
	}
	wg.Wait()
	require.Empty(t, s.shard(ip).entries[ip].active)
}

type partialBackend struct {
	routes map[netip.Prefix]bool
	fail   bool
}

func (b *partialBackend) Start(context.Context) error { return nil }
func (b *partialBackend) List() (map[netip.Prefix]bool, error) {
	m := map[netip.Prefix]bool{}
	for p := range b.routes {
		m[p] = true
	}
	return m, nil
}

func (b *partialBackend) Add(ps []netip.Prefix) error {
	for _, p := range ps {
		b.routes[p] = true
		if b.fail {
			b.fail = false
			return E.New("partial success")
		}
	}
	return nil
}

func (b *partialBackend) Delete(ps []netip.Prefix) error {
	for _, p := range ps {
		delete(b.routes, p)
	}
	return nil
}

func (b *partialBackend) Peers() map[string]bool { return map[string]bool{} }
func (b *partialBackend) Close() error           { return nil }
func TestReconcileAfterPartialFailure(t *testing.T) {
	c := testConfig(t)
	backend := &partialBackend{routes: map[netip.Prefix]bool{}, fail: true}
	s := &Service{
		config:  c,
		state:   newRouteState(c),
		backend: backend,
		logger:  log.NewNOPFactory().Logger(),
	}
	s.configValid.Store(true)
	now := time.Now()
	for _, a := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		s.state.learn(association{
			Domain:  "jp.example",
			Address: netip.MustParseAddr(a),
			Expires: now.Add(time.Minute),
		}, now)
	}
	s.reconcile(now)
	require.Len(t, s.snapshot.Load().Actual, 1)
	require.NotEmpty(t, s.snapshot.Load().LastError)
	s.reconcile(now)
	require.Len(t, s.snapshot.Load().Actual, 2)
	require.Empty(t, s.snapshot.Load().LastError)
	expired := now.Add(2 * time.Minute)
	s.state.expire(expired)
	s.reconcile(expired)
	s.reconcile(expired.Add(2 * time.Second))
	require.Empty(t, s.snapshot.Load().Actual)
}

func BenchmarkWarmLookup(b *testing.B) {
	c := testConfig(b)
	c.options.Limits.MaxDNSAssociations = 100000
	s := newRouteState(c)
	now := time.Now()
	for i := range 10000 {
		ip := netip.AddrFrom4([4]byte{11, byte(i >> 8), byte(i), 1})
		for j := range 10 {
			s.learn(association{
				Domain:  fmt.Sprintf("%d.example", j),
				Address: ip,
				Expires: now.Add(time.Hour),
			}, now)
		}
	}
	ip := netip.MustParseAddr("11.0.1.1")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.lookup(ip, now)
	}
}
