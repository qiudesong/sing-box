package dns

import (
	"context"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"

	"github.com/miekg/dns"
)

func questionType(message *dns.Msg) uint16 {
	if len(message.Question) == 0 {
		return 0
	}
	return message.Question[0].Qtype
}

func responseCode(message *dns.Msg) int {
	if message == nil {
		return 0
	}
	return message.Rcode
}

func (c *Client) exchangeToTransport(ctx context.Context, transport adapter.DNSTransport, message *dns.Msg) (*dns.Msg, error) {
	if c.metrics == nil {
		return c.exchangeToTransportRaw(ctx, transport, message)
	}
	start := time.Now()
	response, err := c.exchangeToTransportRaw(ctx, transport, message)
	c.metrics.ObserveDNS(adapter.DNSMetricsKind(ctx), transport.Tag(), questionType(message), responseCode(response), time.Since(start), err)
	return response, err
}

func (c *Client) exchangeToTransportAsync(ctx context.Context, transport adapter.DNSTransport, message *dns.Msg, callback func(*dns.Msg, error)) {
	if c.metrics == nil {
		c.exchangeToTransportAsyncRaw(ctx, transport, message, callback)
		return
	}
	start := time.Now()
	c.exchangeToTransportAsyncRaw(ctx, transport, message, func(response *dns.Msg, err error) {
		c.metrics.ObserveDNS(adapter.DNSMetricsKind(ctx), transport.Tag(), questionType(message), responseCode(response), time.Since(start), err)
		callback(response, err)
	})
}

func (c *Client) countCache(server string, hit, stale bool) {
	if c.metrics == nil {
		return
	}
	result := C.MetricCacheResultMiss
	if hit {
		result = C.MetricCacheResultHit
		if stale {
			result = C.MetricCacheResultStale
		}
	}
	c.metrics.CountDNSCache(server, result)
}
