package bgpbypass

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	"github.com/sagernet/sing-box/common/observability"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"

	"github.com/stretchr/testify/require"
)

func testService(t testing.TB) *Service {
	c := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Service{
		Adapter:  boxService.NewAdapter(C.TypeBGPBypass, "test"),
		ctx:      ctx,
		cancel:   cancel,
		config:   c,
		state:    newRouteState(c),
		logger:   log.NewNOPFactory().Logger(),
		requests: make(chan learnRequest, 1),
	}
	s.configValid.Store(true)
	s.snapshot.Store(&controlSnapshot{
		Desired: map[netip.Prefix]bool{},
		Actual:  map[netip.Prefix]bool{},
		Peers:   map[string]bool{},
		At:      time.Now(),
	})
	return s
}

func TestStateRestoreReevaluatesPolicyAndBounds(t *testing.T) {
	s := testService(t)
	path := filepath.Join(t.TempDir(), "state.json")
	s.config.options.StatePath = path
	now := time.Now()
	ip := netip.MustParseAddr("8.8.8.8")
	require.NoError(t, writeState(path, savedState{Version: 1, Associations: []association{{
		Domain:  "jp.example",
		Address: ip,
		Expires: now.Add(time.Hour),
	}, {
		Domain:  "expired.example",
		Address: ip,
		Expires: now.Add(-time.Hour),
	}, {
		Domain:  "bad.example",
		Address: netip.MustParseAddr("127.0.0.1"),
		Expires: now.Add(time.Hour),
	}}}))
	s.loadState()
	require.EqualValues(t, 1, s.state.count.Load())
	require.Equal(t, "jp", s.state.lookup(ip, now))
	s.state.pin(ip, "jp")
	s.saveState()
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	other := testService(t)
	other.config.options.StatePath = path
	other.loadState()
	require.Empty(t, other.state.shard(ip).entries[ip].active)
	require.NoError(t, os.WriteFile(path, []byte(`{"version":999,"associations":[]}`), 0o600))
	cold := testService(t)
	cold.config.options.StatePath = path
	cold.loadState()
	require.Zero(t, cold.state.count.Load())
}

func TestHTTPAuthenticationAndPagination(t *testing.T) {
	s := testService(t)
	manager, err := observability.New(option.ObservabilityOptions{
		ListenPort:  9098,
		Diagnostics: true,
		Secret:      "token",
	}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	manager.Add(s)
	s.snapshot.Load().Desired[netip.MustParsePrefix("8.8.8.8/32")] = true
	s.snapshot.Load().Desired[netip.MustParsePrefix("8.8.4.4/32")] = true
	prefix := "/services/" + C.TypeBGPBypass + "/test"
	for _, path := range []string{prefix + "/routes", prefix + "/routes/8.8.8.8", prefix + "/readyz"} {
		w := httptest.NewRecorder()
		manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, prefix+"/routes?limit=1", nil)
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"next":"8.8.4.4/32"`)
	require.Contains(t, w.Body.String(), `"router_fib_confirmed":false`)
	w = httptest.NewRecorder()
	manager.Handler(true).ServeHTTP(w, httptest.NewRequest(http.MethodGet, prefix+"/routes", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestMetricsSnapshot(t *testing.T) {
	s := testService(t)
	s.dnsLatency[1].observe(time.Millisecond)
	s.dnsLatency[1].observe(3 * time.Millisecond)
	var histogram adapter.Metric
	s.CollectMetrics(func(metric adapter.Metric) {
		if metric.Name == "sing_box_bgp_bypass_dns_observe_seconds" && metric.Labels["reason"] == "warm" {
			histogram = metric
		}
	}, nil)
	require.Equal(t, adapter.MetricHistogram, histogram.Type)
	require.EqualValues(t, 2, histogram.Count)
	require.EqualValues(t, 1, histogram.Buckets[0.002])
	require.InDelta(t, 0.004, histogram.Sum, 0.000001)
}

func TestObservationModeDoesNotStartBGP(t *testing.T) {
	s := testService(t)
	s.config.options.Advertise = false
	ip := netip.MustParseAddr("8.8.8.8")
	now := time.Now()
	s.state.learn(association{
		Domain:  "jp.example",
		Address: ip,
		Expires: now.Add(time.Hour),
	}, now)
	s.reconcile(now)
	require.Len(t, s.snapshot.Load().Desired, 1)
	require.Empty(t, s.snapshot.Load().Actual)
	require.Nil(t, s.backend)
}
