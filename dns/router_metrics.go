package dns

import (
	"context"
	"net/netip"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"

	"github.com/miekg/dns"
)

var _ RouterInstance = (*metricsRouter)(nil)

type metricsRouter struct {
	*Router
	metrics adapter.DNSMetricsObserver
}

func (r *metricsRouter) Exchange(ctx context.Context, message *dns.Msg, options adapter.DNSQueryOptions) (*dns.Msg, error) {
	start := time.Now()
	response, err := r.Router.Exchange(ctx, message, options)
	r.metrics.ObserveDNS(C.MetricDNSKindExchange, "", questionType(message), responseCode(response), time.Since(start), err)
	return response, err
}

func (r *metricsRouter) ExchangeAsync(ctx context.Context, message *dns.Msg, options adapter.DNSQueryOptions, callback func(*dns.Msg, error)) {
	start := time.Now()
	r.Router.ExchangeAsync(ctx, message, options, func(response *dns.Msg, err error) {
		r.metrics.ObserveDNS(C.MetricDNSKindExchange, "", questionType(message), responseCode(response), time.Since(start), err)
		callback(response, err)
	})
}

func (r *metricsRouter) Lookup(ctx context.Context, domain string, options adapter.DNSQueryOptions) ([]netip.Addr, error) {
	start := time.Now()
	addresses, err := r.Router.Lookup(ctx, domain, options)
	r.metrics.ObserveDNS(C.MetricDNSKindLookup, "", 0, 0, time.Since(start), err)
	return addresses, err
}
