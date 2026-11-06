//go:build with_prometheus

package dns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/observability"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"

	mDNS "github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func TestMetricsSyncAsyncCacheAndFailure(t *testing.T) {
	manager, err := observability.New(option.ObservabilityOptions{ListenPort: 9098, Metrics: true}, log.NewNOPFactory().Logger())
	require.NoError(t, err)
	ctx := adapter.ContextWithMetricsObserver(context.Background(), manager.Observer)
	transport := &fakeDNSTransport{tag: "final", address: netip.MustParseAddr("8.8.8.8")}
	ctx = service.ContextWith[adapter.DNSTransportManager](ctx, &fakeDNSTransportManager{
		transports:       map[string]adapter.DNSTransport{transport.tag: transport},
		defaultTransport: transport,
	})
	router, err := NewRouter(ctx, log.NewNOPFactory(), option.DNSOptions{
		RawDNSOptions: option.RawDNSOptions{ReverseMapping: true},
	})
	require.NoError(t, err)
	require.IsType(t, (*metricsRouter)(nil), router)
	require.NoError(t, router.Initialize(nil))
	require.NoError(t, router.ValidateRuleSetMetadataUpdate("test", adapter.RuleSetMetadata{}))
	require.True(t, router.DNSCacheExpires())
	scope := adapter.NewScope(ctx, log.NewNOPFactory().Logger())
	t.Cleanup(func() { require.NoError(t, scope.Close()) })
	require.NoError(t, router.Start(adapter.StartStateStart, scope))
	observer := new(testResponseObserver)
	unregister := router.RegisterDNSResponseObserver(observer)
	t.Cleanup(unregister)
	message := new(mDNS.Msg)
	message.SetQuestion("example.org.", mDNS.TypeA)
	_, err = router.Exchange(ctx, message, adapter.DNSQueryOptions{})
	require.NoError(t, err)
	done := make(chan error, 1)
	router.ExchangeAsync(ctx, message, adapter.DNSQueryOptions{}, func(_ *mDNS.Msg, err error) { done <- err })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("callback not called")
	}
	require.EqualValues(t, 2, observer.calls.Load())
	domain, loaded := router.LookupReverseMapping(transport.address)
	require.True(t, loaded)
	require.Equal(t, "example.org", domain)
	addresses, err := router.Lookup(ctx, "example.org", adapter.DNSQueryOptions{Strategy: C.DomainStrategyIPv4Only})
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{transport.address}, addresses)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = router.Exchange(ctx, message, adapter.DNSQueryOptions{})
	require.Error(t, err)
	router.ExchangeAsync(ctx, message, adapter.DNSQueryOptions{}, func(_ *mDNS.Msg, err error) { done <- err })
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("callback not called")
	}
	w := httptest.NewRecorder()
	manager.Handler(false).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `sing_box_dns_requests_total{kind="exchange",query_type="1",result="0",server=""} 2`)
	require.Contains(t, w.Body.String(), `sing_box_dns_requests_total{kind="exchange",query_type="1",result="canceled",server=""} 2`)
	require.Contains(t, w.Body.String(), `sing_box_dns_requests_total{kind="lookup",query_type="0",result="0",server=""} 1`)
	require.Contains(t, w.Body.String(), `sing_box_dns_requests_total{kind="upstream",query_type="1",result="0",server="final"} 1`)
	require.Contains(t, w.Body.String(), `sing_box_dns_cache_total{result="hit",server="final"} 2`)
	require.Contains(t, w.Body.String(), `sing_box_dns_cache_total{result="miss",server="final"} 1`)
}
