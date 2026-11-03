package dns

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"

	mDNS "github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

type testResponseObserver struct {
	calls   atomic.Int32
	inbound atomic.Value
	ttl     atomic.Uint32
}

func (o *testResponseObserver) ObserveDNSResponse(ctx context.Context, q, r *mDNS.Msg) {
	o.calls.Add(1)
	if len(r.Answer) > 0 {
		o.ttl.Store(r.Answer[0].Header().Ttl)
	}
	if m := adapter.ContextFrom(ctx); m != nil {
		o.inbound.Store(m.Inbound)
	}
}

func TestFinalResponseObserverSyncAsyncAndCache(t *testing.T) {
	transport := &fakeDNSTransport{
		tag:     "final",
		rcode:   mDNS.RcodeSuccess,
		address: netip.MustParseAddr("8.8.8.8"),
	}
	router := raceTestRouter(t, transport)
	router.client = NewClient(ClientOptions{Context: context.Background(), Logger: log.NewNOPFactory().Logger()})
	observer := new(testResponseObserver)
	unregister := router.RegisterDNSResponseObserver(observer)
	q := new(mDNS.Msg)
	q.SetQuestion("example.org.", mDNS.TypeA)
	ctx := adapter.WithContext(context.Background(), &adapter.InboundContext{Inbound: "lan"})
	_, err := router.Exchange(ctx, q, adapter.DNSQueryOptions{})
	require.NoError(t, err)
	done := make(chan error, 1)
	router.ExchangeAsync(ctx, q, adapter.DNSQueryOptions{}, func(_ *mDNS.Msg, err error) { done <- err })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("callback not called")
	}
	require.EqualValues(t, 2, observer.calls.Load())
	require.Equal(t, "lan", observer.inbound.Load())
	require.EqualValues(t, 1, transport.queryCount.Load())
	unregister()
	unregister()
	_, err = router.Exchange(ctx, q, adapter.DNSQueryOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 2, observer.calls.Load())
}

func TestObserverIgnoresSpeculativeLoser(t *testing.T) {
	slow := &fakeDNSTransport{
		tag:     "slow",
		delay:   80 * time.Millisecond,
		rcode:   mDNS.RcodeSuccess,
		address: netip.MustParseAddr("8.8.8.8"),
	}
	fast := &fakeDNSTransport{
		tag:     "fast",
		delay:   time.Millisecond,
		rcode:   mDNS.RcodeSuccess,
		address: netip.MustParseAddr("8.8.4.4"),
	}
	r := raceTestRouter(t, slow, fast)
	r.rules = raceTestRules(t, []option.DNSRule{evaluateRule("slow", "slow", false), evaluateRule("fast", "fast", false), respondRule("slow", true, true), respondRule("fast", true, true)})
	observer := new(testResponseObserver)
	r.RegisterDNSResponseObserver(observer)
	q := new(mDNS.Msg)
	q.SetQuestion("example.org.", mDNS.TypeA)
	done := make(chan *mDNS.Msg, 1)
	r.ExchangeAsync(context.Background(), q, adapter.DNSQueryOptions{}, func(response *mDNS.Msg, err error) {
		if err == nil {
			done <- response
		} else {
			done <- nil
		}
	})
	select {
	case response := <-done:
		require.Equal(t, fast.address, responseAddress(t, response))
	case <-time.After(time.Second):
		t.Fatal("callback timeout")
	}
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 1, observer.calls.Load())
}

func TestObserverPredefinedSyncAndAsync(t *testing.T) {
	router := raceTestRouter(t, &fakeDNSTransport{tag: "unused"})
	var rules []option.DNSRule
	require.NoError(t, json.Unmarshal([]byte(`[{"domain":["example.org"],"action":"predefined","answer":["example.org. 17 IN A 8.8.8.8"]}]`), &rules))
	router.rules = raceTestRules(t, rules)
	observer := new(testResponseObserver)
	router.RegisterDNSResponseObserver(observer)
	q := new(mDNS.Msg)
	q.SetQuestion("example.org.", mDNS.TypeA)
	response, err := router.Exchange(context.Background(), q, adapter.DNSQueryOptions{})
	require.NoError(t, err)
	require.Len(t, response.Answer, 1)
	require.EqualValues(t, 17, observer.ttl.Load())
	done := make(chan error, 1)
	router.ExchangeAsync(context.Background(), q, adapter.DNSQueryOptions{}, func(_ *mDNS.Msg, err error) { done <- err })
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("predefined callback timeout")
	}
	require.EqualValues(t, 2, observer.calls.Load())
}

func TestObserverCacheUsesRemainingTTL(t *testing.T) {
	transport := &fakeDNSTransport{
		tag:     "final",
		rcode:   mDNS.RcodeSuccess,
		address: netip.MustParseAddr("8.8.8.8"),
	}
	router := raceTestRouter(t, transport)
	router.client = NewClient(ClientOptions{Context: context.Background(), Logger: log.NewNOPFactory().Logger()})
	observer := new(testResponseObserver)
	router.RegisterDNSResponseObserver(observer)
	q := new(mDNS.Msg)
	q.SetQuestion("example.org.", mDNS.TypeA)
	_, err := router.Exchange(context.Background(), q, adapter.DNSQueryOptions{})
	require.NoError(t, err)
	initial := observer.ttl.Load()
	time.Sleep(1100 * time.Millisecond)
	_, err = router.Exchange(context.Background(), q, adapter.DNSQueryOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 1, transport.queryCount.Load())
	require.Less(t, observer.ttl.Load(), initial)
}
