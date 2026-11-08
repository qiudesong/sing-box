package bgpbypass

import (
	"context"
	"time"

	"github.com/sagernet/sing-box/adapter"

	"github.com/miekg/dns"
)

func (s *Service) startDNSProbe() {
	// Probe the resolver without populating the client DNS learning state.
	s.wg.Go(func() {
		probe := func() {
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			defer cancel()
			q := new(dns.Msg)
			q.SetQuestion(".", dns.TypeNS)
			response, err := s.dns.Exchange(adapter.WithContext(ctx, &adapter.InboundContext{}), q, adapter.DNSQueryOptions{DisableCache: true})
			s.dnsReady.Store(err == nil && response != nil && response.Rcode == dns.RcodeSuccess && len(response.Answer) > 0)
		}
		probe()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				probe()
			}
		}
	})
}
