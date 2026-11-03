package bgpbypass

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"

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
	s.config.options.Observability.Secret = "token"
	s.config.options.Observability.Diagnostics = true
	s.config.options.Observability.Metrics = true
	s.snapshot.Load().Desired[netip.MustParsePrefix("8.8.8.8/32")] = true
	s.snapshot.Load().Desired[netip.MustParsePrefix("8.8.4.4/32")] = true
	for _, path := range []string{"/metrics", "/routes", "/routes/8.8.8.8", "/readyz"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.handler(false).ServeHTTP(w, r)
		require.Equal(t, 401, w.Code)
	}
	r := httptest.NewRequest("GET", "/routes?limit=1", nil)
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.handler(false).ServeHTTP(w, r)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"next":"8.8.4.4/32"`)
	require.Contains(t, w.Body.String(), `"router_fib_confirmed":false`)
	r = httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("Authorization", "Bearer token")
	w = httptest.NewRecorder()
	s.handler(false).ServeHTTP(w, r)
	require.NotContains(t, w.Body.String(), "8.8.8.8")
	require.Contains(t, w.Body.String(), "desired_routes")
	s.dnsLatency[1].observe(time.Millisecond)
	s.dnsLatency[1].observe(3 * time.Millisecond)
	w = httptest.NewRecorder()
	s.handler(false).ServeHTTP(w, r)
	require.Contains(t, w.Body.String(), `dns_observe_seconds_bucket{service="test",reason="warm",le="0.002"} 1`)
	require.Contains(t, w.Body.String(), `dns_observe_seconds_bucket{service="test",reason="warm",le="+Inf"} 2`)
	require.Contains(t, w.Body.String(), `dns_observe_seconds_count{service="test",reason="warm"} 2`)
	w = httptest.NewRecorder()
	s.handler(true).ServeHTTP(w, httptest.NewRequest("GET", "/routes", nil))
	require.Equal(t, 404, w.Code)
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
