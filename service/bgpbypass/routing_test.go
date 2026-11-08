package bgpbypass

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"

	"github.com/stretchr/testify/require"
	"go4.org/netipx"
)

type nativeTestRouter struct {
	adapter.Router
	rules []adapter.Rule
	sets  map[string]adapter.RuleSet
}

func (r *nativeTestRouter) Rules() []adapter.Rule { return r.rules }
func (r *nativeTestRouter) RuleSet(tag string) (adapter.RuleSet, bool) {
	set, found := r.sets[tag]
	return set, found
}

type nativeTestOutbound struct {
	outbound.Adapter
	N.Dialer
}

type nativeTestOutbounds struct {
	adapter.OutboundManager
	values map[string]adapter.Outbound
	final  string
}

func (m *nativeTestOutbounds) Default() adapter.Outbound { return m.values[m.final] }
func (m *nativeTestOutbounds) Outbound(tag string) (adapter.Outbound, bool) {
	out, found := m.values[tag]
	return out, found
}

type nativeTestServices struct {
	adapter.ServiceManager
	control *Service
}

type mutableNativeSet struct {
	adapter.RuleSet
	access  sync.RWMutex
	current adapter.RuleSet
}

func (s *mutableNativeSet) Match(metadata *adapter.InboundContext) bool {
	s.access.RLock()
	defer s.access.RUnlock()
	return s.current.Match(metadata)
}

func (s *mutableNativeSet) Metadata() adapter.RuleSetMetadata {
	s.access.RLock()
	defer s.access.RUnlock()
	return s.current.Metadata()
}

func (s *mutableNativeSet) ExtractIPSet() []*netipx.IPSet {
	s.access.RLock()
	defer s.access.RUnlock()
	return s.current.ExtractIPSet()
}

func (m nativeTestServices) Get(tag string) (adapter.Service, bool) {
	return m.control, tag == m.control.Tag()
}

func (m nativeTestServices) Services() []adapter.Service {
	return []adapter.Service{m.control}
}

func nativeService(t *testing.T, raw string) (*Service, []adapter.Rule, error) {
	t.Helper()
	s := testService(t)
	s.config.options.Advertise = true
	s.config.rules = nil
	s.config.outbounds = nil
	router := &nativeTestRouter{sets: map[string]adapter.RuleSet{}}
	manager := &nativeTestOutbounds{values: map[string]adapter.Outbound{}, final: "direct"}
	for _, tag := range []string{"direct", "proxy", "jp"} {
		outboundType := C.TypeSOCKS
		if tag == "direct" {
			outboundType = C.TypeDirect
		}
		manager.values[tag] = &nativeTestOutbound{
			Adapter: outbound.NewAdapter(outboundType, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		}
	}
	s.router, s.outbounds = router, manager
	ctx := service.ContextWith[adapter.Router](context.Background(), router)
	ctx = service.ContextWith[adapter.ServiceManager](ctx, nativeTestServices{control: s})
	var options option.RouteOptions
	require.NoError(t, json.Unmarshal([]byte(raw), &options))
	if options.Final != "" {
		manager.final = options.Final
	}
	for _, set := range options.RuleSet {
		for _, tag := range set.Tag {
			compiled, err := R.NewRuleSet(ctx, log.NewNOPFactory().Logger(), tag, set)
			require.NoError(t, err)
			router.sets[tag] = &mutableNativeSet{RuleSet: compiled, current: compiled}
			t.Cleanup(func() { _ = compiled.Close() })
		}
	}
	for _, rawRule := range options.Rules {
		rule, err := R.NewRule(ctx, log.NewNOPFactory().Logger(), rawRule, false)
		require.NoError(t, err)
		router.rules = append(router.rules, rule)
		require.NoError(t, rule.Start())
		t.Cleanup(func() { _ = rule.Close() })
	}
	err := s.compileRouting(option.Options{Route: &options})
	s.running.Store(true)
	return s, router.rules, err
}

func nativeSelection(rules []adapter.Rule, metadata adapter.InboundContext) string {
	for _, rule := range rules {
		metadata.ResetRuleCache()
		if rule.Match(&metadata) {
			if route, ok := rule.Action().(*R.RuleActionRoute); ok {
				return route.Outbound
			}
			return "rejected"
		}
	}
	return "direct"
}

func TestNativeLearningAndConnectionRouting(t *testing.T) {
	s, rules, err := nativeService(t, `{"rules":[
		{"port":123,"outbound":"direct"},
		{"bgp_bypass":"test","outbound":"jp"},
		{"bgp_bypass":"test","outbound":"proxy"},
		{"domain_suffix":"jp.example","outbound":"jp"},
		{"domain_suffix":"example","outbound":"proxy"}
	]}`)
	require.NoError(t, err)
	require.Equal(t, []int{0}, s.config.skipped)
	now := time.Now()
	for _, address := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		ip := netip.MustParseAddr(address)
		require.True(t, s.state.learn(association{Domain: "a.jp.example", Address: ip, Expires: now.Add(time.Hour)}, now))
		metadata := adapter.InboundContext{Inbound: "lan", Destination: M.Socksaddr{Addr: ip, Port: 443}}
		require.Equal(t, "jp", nativeSelection(rules, metadata))
		metadata.Destination.Port = 123
		require.Equal(t, "direct", nativeSelection(rules, metadata), "native exceptions precede learned matches")
		metadata.Destination.Port = 443
		s.config.options.Advertise = false
		require.Equal(t, "direct", nativeSelection(rules, metadata))
		s.config.options.Advertise = true
		s.configValid.Store(false)
		require.Equal(t, "direct", nativeSelection(rules, metadata))
		s.configValid.Store(true)
	}
}

func TestNativePreflightDoesNotBroadenUnavailableConditions(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"domain_suffix":"example","port":443,"outbound":"jp"},
		{"type":"logical","mode":"or","rules":[{"domain_suffix":"example"},{"process_name":"app"}],"outbound":"jp"},
		{"domain_suffix":"blocked.example","action":"reject"},
		{"bgp_bypass":"test","outbound":"proxy"},
		{"domain_suffix":"example","outbound":"proxy"}
	]}`)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, s.config.skipped)
	ip := netip.MustParseAddr("8.8.8.8")
	require.Equal(t, "proxy", s.config.decide("a.example", ip).Outbound)
	require.Empty(t, s.config.decide("blocked.example", ip).Outbound)
	// The preflight never learns from its own dynamic membership, including
	// after another domain has already created a route for the same address.
	now := time.Now()
	s.state.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now)
	require.Empty(t, s.config.decide("unmatched.test", ip).Outbound)
}

func TestNativePreflightPreservesRuleSemantics(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"jp"},
		{"bgp_bypass":"test","outbound":"proxy"},
		{"type":"logical","mode":"and","rules":[{"domain_suffix":"example"},{"ip_cidr":"8.8.8.0/24"}],"outbound":"jp"},
		{"domain":"exact.test","domain_suffix":"other.test","outbound":"proxy"}
	]}`)
	require.NoError(t, err)
	require.Equal(t, "jp", s.config.decide("a.example", netip.MustParseAddr("8.8.8.8")).Outbound)
	require.Empty(t, s.config.decide("a.example", netip.MustParseAddr("8.8.4.4")).Outbound)
	for _, domain := range []string{"exact.test", "a.other.test"} {
		require.Equal(t, "proxy", s.config.decide(domain, netip.MustParseAddr("8.8.8.8")).Outbound, "native domain forms use OR")
	}
}

func TestNativePreflightFinalAndBindingValidation(t *testing.T) {
	s, _, err := nativeService(t, `{"final":"proxy","rules":[{"bgp_bypass":"test","outbound":"proxy"}]}`)
	require.NoError(t, err)
	require.Equal(t, "proxy", s.config.decide("unmatched.test", netip.MustParseAddr("8.8.8.8")).Outbound)
	for _, raw := range []string{
		`{"rules":[{"domain_suffix":"example","outbound":"proxy"}]}`,
		`{"final":"proxy","rules":[]}`,
		`{"rules":[{"network":"tcp","bgp_bypass":"test","outbound":"proxy"},{"domain_suffix":"example","outbound":"proxy"}]}`,
		`{"rules":[{"inbound":"other","bgp_bypass":"test","outbound":"proxy"},{"domain_suffix":"example","outbound":"proxy"}]}`,
	} {
		_, _, err := nativeService(t, raw)
		require.Error(t, err)
	}
}

type nativeTestEndpoint struct {
	*nativeTestOutbound
}

func (*nativeTestEndpoint) Start(adapter.StartStage, *adapter.Scope) error { return nil }

type nativeTestEndpoints struct {
	adapter.EndpointManager
	endpoint adapter.Endpoint
}

func (m *nativeTestEndpoints) Get(tag string) (adapter.Endpoint, bool) {
	if tag == m.endpoint.Tag() {
		return m.endpoint, true
	}
	return nil, false
}

func TestNativePreflightUninitializedEndpointDefault(t *testing.T) {
	s := testService(t)
	endpoint := &nativeTestEndpoint{
		nativeTestOutbound: &nativeTestOutbound{
			Adapter: outbound.NewAdapter(C.TypeWireGuard, "endpoint", []string{N.NetworkTCP, N.NetworkUDP}, nil),
		},
	}
	manager := outbound.NewManager(nil, &nativeTestEndpoints{endpoint: endpoint}, "endpoint")
	require.Nil(t, manager.Default())
	s.outbounds = manager
	router := &nativeTestRouter{}
	s.router = router
	var options option.RouteOptions
	require.NoError(t, json.Unmarshal([]byte(`{"final":"endpoint","rules":[
		{"bgp_bypass":"test","outbound":"endpoint"},
		{"domain_suffix":"example","action":"route"}
	]}`), &options))
	for _, raw := range options.Rules {
		rule, err := R.NewRule(context.Background(), log.NewNOPFactory().Logger(), raw, false)
		require.NoError(t, err)
		router.rules = append(router.rules, rule)
		t.Cleanup(func() { _ = rule.Close() })
	}
	require.NoError(t, s.compileRouting(option.Options{Route: &options}))
	ip := netip.MustParseAddr("8.8.8.8")
	require.Equal(t, "endpoint", s.config.decide("a.example", ip).Outbound)
	require.Equal(t, "endpoint", s.config.decide("unmatched.test", ip).Outbound)
	require.Nil(t, manager.Default(), "preflight must not start the outbound manager")
}

func TestNativeRuleSetRefreshAndValidation(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"proxy"},
		{"rule_set":"domains","outbound":"proxy"}
	],"rule_set":[{"type":"inline","tag":"domains","rules":[{"domain_suffix":"example"}]}]}`)
	require.NoError(t, err)
	ip := netip.MustParseAddr("8.8.8.8")
	now := time.Now()
	s.state.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now)
	require.Equal(t, "proxy", s.state.lookup(ip, now))
	require.Error(t, s.ValidateRuleSetUpdate("domains", adapter.RuleSetMetadata{ContainsNonDNSBypassRule: true}))
	// Simulate a rule-set's atomic content replacement. Both native routing
	// and preflight retain the same rule and set objects.
	set := s.config.sets["domains"].(*mutableNativeSet)
	var replacement option.RuleSet
	require.NoError(t, json.Unmarshal([]byte(`{"type":"inline","tag":"domains","rules":[{"domain_suffix":"changed.test"}]}`), &replacement))
	updated, err := R.NewRuleSet(context.Background(), log.NewNOPFactory().Logger(), "domains", replacement)
	require.NoError(t, err)
	t.Cleanup(func() { _ = updated.Close() })
	set.access.Lock()
	set.current = updated
	set.access.Unlock()
	require.NoError(t, s.refreshRules())
	require.Empty(t, s.state.lookup(ip, now))
	require.Equal(t, "proxy", s.config.decide("a.changed.test", ip).Outbound)
}

func TestNativeRuleSetReplacementAtCapacity(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Minute} {
		for _, active := range []bool{false, true} {
			name := delay.String()
			if active {
				name += "/active"
			}
			t.Run(name, func(t *testing.T) {
				s, _, err := nativeService(t, `{"rules":[
					{"bgp_bypass":"test","outbound":"proxy"},
					{"rule_set":"domains","outbound":"proxy"}
				],"rule_set":[{"type":"inline","tag":"domains","rules":[{"domain_suffix":"old.example"}]}]}`)
				require.NoError(t, err)
				s.config.options.Limits.MaxDynamicRoutes = 1
				s.state.limitsRoutes = 1
				s.state.withdrawDelay = delay
				s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
				now := time.Now()
				a := netip.MustParseAddr("8.8.8.8")
				b := netip.MustParseAddr("8.8.4.4")
				require.True(t, s.state.learn(association{
					Domain:  "a.old.example",
					Address: a,
					Expires: now.Add(time.Hour),
				}, now))
				require.True(t, s.state.learn(association{
					Domain:  "b.new.example",
					Address: b,
					Expires: now.Add(time.Hour),
				}, now))
				s.reconcile(now)
				require.True(t, s.snapshot.Load().Actual[netip.PrefixFrom(a, 32)])
				var release func()
				if active {
					release = s.state.pin(a, "proxy")
					defer release()
				}
				set := s.config.sets["domains"].(*mutableNativeSet)
				var replacement option.RuleSet
				require.NoError(t, json.Unmarshal([]byte(`{"type":"inline","tag":"domains","rules":[{"domain_suffix":"new.example"}]}`), &replacement))
				updated, err := R.NewRuleSet(context.Background(), log.NewNOPFactory().Logger(), "domains", replacement)
				require.NoError(t, err)
				defer updated.Close()
				set.access.Lock()
				set.current = updated
				set.access.Unlock()
				require.NoError(t, s.refreshRules())
				require.Equal(t, "proxy", winner(s.state.shard(b).entries[b], now).Outbound)
				s.reconcile(now)
				if active || delay > 0 {
					require.True(t, s.snapshot.Load().Actual[netip.PrefixFrom(a, 32)])
					require.False(t, s.snapshot.Load().Actual[netip.PrefixFrom(b, 32)])
					require.Empty(t, s.state.lookup(b, now), "pending decisions must not match before admission")
					pendingRelease, acquired := s.state.acquire(b, "proxy")
					require.False(t, acquired)
					require.Nil(t, pendingRelease)
				}
				if active {
					release()
					now = now.Add(time.Second)
					s.reconcile(now)
				}
				s.state.expire(now.Add(delay))
				s.reconcile(now.Add(delay))
				require.Equal(t, map[netip.Prefix]bool{netip.PrefixFrom(b, 32): true}, s.snapshot.Load().Actual)
				require.Equal(t, s.snapshot.Load().Desired, s.snapshot.Load().Actual)
				require.EqualValues(t, 1, s.state.routeSlots.Load())
				require.Equal(t, "proxy", s.state.lookup(b, now.Add(delay)))
			})
		}
	}
}

func TestKnownLeasePolicyChangeCommitsAdmission(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"proxy"},
		{"rule_set":"domains","outbound":"proxy"}
	],"rule_set":[{"type":"inline","tag":"domains","rules":[{"domain_suffix":"old.example"}]}]}`)
	require.NoError(t, err)
	s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
	value := association{
		Domain:  "b.new.example",
		Address: netip.MustParseAddr("8.8.4.4"),
		Expires: time.Now().Add(time.Hour),
	}
	require.False(t, processLearning(s, []association{value}))
	require.True(t, s.snapshot.Load().Converged)
	require.Empty(t, s.snapshot.Load().Actual)
	set := s.config.sets["domains"].(*mutableNativeSet)
	var replacement option.RuleSet
	require.NoError(t, json.Unmarshal([]byte(`{"type":"inline","tag":"domains","rules":[{"domain_suffix":"new.example"}]}`), &replacement))
	updated, err := R.NewRuleSet(context.Background(), log.NewNOPFactory().Logger(), "domains", replacement)
	require.NoError(t, err)
	defer updated.Close()
	set.access.Lock()
	set.current = updated
	set.access.Unlock()
	// A queued rule-set notification may follow a DNS request. Learning uses
	// the set's current contents even before the controller handles the update.
	require.True(t, s.state.known(value, time.Now()))
	require.True(t, processLearning(s, []association{value}))
	require.True(t, s.snapshot.Load().Actual[netip.PrefixFrom(value.Address, 32)])
}

func TestNativeTrackerProtectsActualOutbound(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"proxy"},
		{"domain_suffix":"example","outbound":"proxy"}
	]}`)
	require.NoError(t, err)
	ip := netip.MustParseAddr("8.8.8.8")
	now := time.Now()
	s.state.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Second)}, now)
	metadata := adapter.InboundContext{Inbound: "lan", Destination: M.Socksaddr{Addr: ip}}
	actual, _ := s.outbounds.Outbound("jp")
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	tracked := s.RoutedConnection(context.Background(), client, metadata, nil, actual)
	t.Cleanup(func() { _ = tracked.Close() })
	require.Implements(t, (*interface{ ReaderReplaceable() bool })(nil), tracked)
	s.state.expire(now.Add(2 * time.Second))
	require.True(t, s.state.desired(now.Add(2 * time.Second))[netip.PrefixFrom(ip, 32)])
	require.Equal(t, 1, s.state.shard(ip).entries[ip].active["jp"])
	require.NoError(t, tracked.Close())
	_ = tracked.Close()
	require.Empty(t, s.state.shard(ip).entries[ip].active)
	s.state.desired(now.Add(2 * time.Second))
	require.False(t, s.state.desired(now.Add(4 * time.Second))[netip.PrefixFrom(ip, 32)])
}

func TestNativeTrackerDoesNotAcquireBindingTwice(t *testing.T) {
	s, rules, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"proxy"},
		{"domain_suffix":"example","outbound":"proxy"}
	]}`)
	require.NoError(t, err)
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	require.True(t, s.state.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Hour)}, now))
	metadata := adapter.InboundContext{Inbound: "lan", Destination: M.Socksaddr{Addr: ip}}
	binding := rules[0].(adapter.BGPBypassRule)
	release, acquired := binding.AcquireBGPBypass(&metadata)
	require.True(t, acquired)
	defer release()
	actual, _ := s.outbounds.Outbound("proxy")
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	require.Same(t, client, s.RoutedConnection(context.Background(), client, metadata, rules[0], actual))
	packet := &nativeTestPacketConn{}
	require.Same(t, packet, s.RoutedPacketConnection(context.Background(), packet, metadata, rules[0], actual))
	require.Equal(t, 1, s.state.shard(ip).entries[ip].active["proxy"])
}

type nativeTestPacketConn struct {
	N.PacketConn
}

func (*nativeTestPacketConn) Close() error { return nil }

func TestNativeUDPTrackerAndIgnoredConnections(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"proxy"},
		{"domain_suffix":"example","outbound":"proxy"}
	]}`)
	require.NoError(t, err)
	ip := netip.MustParseAddr("2606:4700:4700::1111")
	now := time.Now()
	s.state.learn(association{Domain: "a.example", Address: ip, Expires: now.Add(time.Second)}, now)
	metadata := adapter.InboundContext{Inbound: "lan", Destination: M.Socksaddr{Addr: ip}}
	actual, _ := s.outbounds.Outbound("proxy")
	conn := &nativeTestPacketConn{}
	tracked := s.RoutedPacketConnection(context.Background(), conn, metadata, nil, actual)
	require.NotSame(t, conn, tracked)
	require.Equal(t, 1, s.state.shard(ip).entries[ip].active["proxy"])
	require.NoError(t, tracked.Close())
	require.NoError(t, tracked.Close())
	require.Empty(t, s.state.shard(ip).entries[ip].active)
	direct, _ := s.outbounds.Outbound("direct")
	require.Same(t, conn, s.RoutedPacketConnection(context.Background(), conn, metadata, nil, direct))
	metadata.Inbound = "internal"
	require.Same(t, conn, s.RoutedPacketConnection(context.Background(), conn, metadata, nil, actual))
	metadata.Inbound = "lan"
	metadata.Destination.Addr = netip.MustParseAddr("8.8.4.4")
	require.Same(t, conn, s.RoutedPacketConnection(context.Background(), conn, metadata, nil, actual))
	metadata.Destination = M.Socksaddr{Fqdn: "unresolved.example"}
	require.Same(t, conn, s.RoutedPacketConnection(context.Background(), conn, metadata, nil, actual))
}
