package bgpbypass

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"

	"github.com/stretchr/testify/require"
)

func TestPendingLeaseRefreshSurvivesCapacityWait(t *testing.T) {
	s, _, err := nativeService(t, `{"rules":[
		{"bgp_bypass":"test","outbound":"proxy"},
		{"rule_set":"domains","outbound":"proxy"}
	],"rule_set":[{"type":"inline","tag":"domains","rules":[{"domain_suffix":"old.example"}]}]}`)
	require.NoError(t, err)
	s.state.limitsRoutes = 1
	s.state.limitsAssociations = 2
	s.state.withdrawDelay = 0
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
		Expires: now.Add(time.Second),
	}, now))
	s.reconcile(now)
	release := s.state.pin(a, "proxy")
	defer release()
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
	s.reconcile(now)
	refreshed := association{
		Domain:  "b.new.example",
		Address: b,
		Expires: now.Add(time.Minute),
	}
	processLearning(s, []association{refreshed})
	require.True(t, s.state.known(refreshed, now.Add(2*time.Second)))
	require.EqualValues(t, 0, s.stats.capacityRejected.Load())
	require.EqualValues(t, 1, s.state.routeSlots.Load())
	require.EqualValues(t, 2, s.state.count.Load())
	require.Len(t, s.state.shard(b).expiry, 1)
	require.False(t, s.state.learn(association{
		Domain:  "other.new.example",
		Address: b,
		Expires: now.Add(time.Minute),
	}, now), "refreshing existing leases must not permit new associations past capacity")
	require.False(t, s.state.learn(association{
		Domain:  "c.new.example",
		Address: netip.MustParseAddr("8.8.4.5"),
		Expires: now.Add(time.Minute),
	}, now), "refreshing pending leases must not allocate extra route slots")
	s.state.expire(now.Add(2 * time.Second))
	release()
	s.reconcile(now.Add(2 * time.Second))
	require.Equal(t, map[netip.Prefix]bool{netip.PrefixFrom(b, 32): true}, s.snapshot.Load().Actual)
	require.EqualValues(t, 1, s.state.routeSlots.Load())
}

func TestEmptyRuleSetUpdateWithdrawsRoutes(t *testing.T) {
	for _, kind := range []string{"learned", "persistent"} {
		t.Run(kind, func(t *testing.T) {
			initial := `{"domain_suffix":"example"}`
			if kind == "persistent" {
				initial = `{"ip_cidr":["8.8.8.0/24"]}`
			}
			s, _, err := nativeService(t, `{"rules":[
				{"bgp_bypass":"test","outbound":"proxy"},
				{"rule_set":"domains","outbound":"proxy"}
			],"rule_set":[{"type":"inline","tag":"domains","rules":[`+initial+`]}]}`)
			require.NoError(t, err)
			s.state.withdrawDelay = 0
			s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
			if kind == "persistent" {
				s.config.fixedSets["domains"] = true
				s.config.options.PersistentRoutes = []option.BGPBypassPersistentRoute{{
					RuleSet:  []string{"domains"},
					Outbound: "proxy",
				}}
			}
			require.NoError(t, s.refreshRules())
			now := time.Now()
			value := association{
				Domain:  "a.example",
				Address: netip.MustParseAddr("8.8.8.8"),
				Expires: now.Add(time.Hour),
			}
			if kind == "learned" {
				require.True(t, s.state.learn(value, now))
			}
			s.reconcile(now)
			require.Len(t, s.snapshot.Load().Actual, 1)
			path := filepath.Join(t.TempDir(), "domains.json")
			require.NoError(t, os.WriteFile(path, []byte(`{"version":3,"rules":[]}`), 0o600))
			ctx := service.ContextWith[adapter.ServiceManager](context.Background(), nativeTestServices{control: s})
			updated, err := R.NewRuleSet(ctx, log.NewNOPFactory().Logger(), "domains", option.RuleSet{
				Type:         C.RuleSetTypeLocal,
				Format:       C.RuleSetFormatSource,
				LocalOptions: option.LocalRuleSet{Path: path},
			})
			require.NoError(t, err, "loading empty content must pass the service's rule-set validator")
			defer updated.Close()
			require.NotEqual(t, adapter.RuleSetMetadata{}, updated.Metadata())
			require.True(t, updated.Metadata().IPCIDRUnion)
			require.False(t, updated.Match(&adapter.InboundContext{}))
			set := s.config.sets["domains"].(*mutableNativeSet)
			set.access.Lock()
			set.current = updated
			set.access.Unlock()
			require.NoError(t, s.refreshRules())
			s.reconcile(now)
			require.Empty(t, s.snapshot.Load().Actual)
			require.True(t, s.snapshot.Load().Converged)
			require.Empty(t, s.state.lookup(value.Address, now))
			if kind == "learned" {
				require.True(t, s.state.known(value, now), "empty policy must withdraw matches without discarding valid DNS leases")
			}
		})
	}
}
