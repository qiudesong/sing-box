package bgpbypass

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"

	"github.com/miekg/dns"
)

func domainName(s string) string { return strings.ToLower(strings.TrimSuffix(s, ".")) }

// DNS graph traversal only trusts records reachable from the question. TTLs
// include every alias edge; unrelated additional/glue addresses are ignored.
func responseAssociations(query, response *dns.Msg, now time.Time) []association {
	if query == nil || response == nil || len(query.Question) != 1 || response.Rcode != dns.RcodeSuccess || response.Truncated {
		return nil
	}
	q := query.Question[0]
	if q.Qclass != dns.ClassINET {
		return nil
	}
	if !slices.Contains([]uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeSRV}, q.Qtype) {
		return nil
	}
	root := domainName(q.Name)
	reachable := map[string]uint32{root: ^uint32(0)}
	additionalTargets := map[string]uint32{}
	// Limit input work even when a transport returns an unusually large answer.
	if len(response.Answer)+len(response.Extra) > 2048 {
		return nil
	}
	for range 16 {
		changed := false
		for _, rr := range response.Answer {
			h := rr.Header()
			ttl, ok := reachable[domainName(h.Name)]
			if !ok || h.Class != dns.ClassINET {
				continue
			}
			ttl = min(ttl, h.Ttl)
			var target string
			switch v := rr.(type) {
			case *dns.CNAME:
				target = v.Target
			case *dns.SRV:
				target = v.Target
			case *dns.SVCB:
				target = v.Target
				if target == "." && v.Priority > 0 {
					target = h.Name
				}
			case *dns.HTTPS:
				target = v.Target
				if target == "." && v.Priority > 0 {
					target = h.Name
				}
			}
			if target == "" || target == "." {
				continue
			}
			target = domainName(target)
			if old, ok := additionalTargets[target]; !ok || ttl < old {
				additionalTargets[target] = ttl
			}
			if old, ok := reachable[target]; !ok || ttl < old {
				reachable[target] = ttl
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	addresses := map[netip.Addr]uint32{}
	add := func(raw []byte, ttl uint32) {
		ip, ok := netip.AddrFromSlice(raw)
		if !ok {
			return
		}
		ip = ip.Unmap()
		if old, ok := addresses[ip]; !ok || ttl < old {
			addresses[ip] = ttl
		}
	}
	for sectionIndex, section := range [][]dns.RR{response.Answer, response.Extra} {
		for _, rr := range section {
			h := rr.Header()
			ttl, ok := reachable[domainName(h.Name)]
			if !ok || h.Class != dns.ClassINET {
				continue
			}
			if sectionIndex == 1 {
				refTTL, referenced := additionalTargets[domainName(h.Name)]
				if !referenced || (h.Rrtype != dns.TypeA && h.Rrtype != dns.TypeAAAA) {
					continue
				}
				ttl = min(ttl, refTTL)
			}
			ttl = min(ttl, h.Ttl)
			var values []dns.SVCBKeyValue
			switch v := rr.(type) {
			case *dns.A:
				add(v.A, ttl)
			case *dns.AAAA:
				add(v.AAAA, ttl)
			case *dns.SVCB:
				values = v.Value
			case *dns.HTTPS:
				values = v.Value
			}
			for _, value := range values {
				switch v := value.(type) {
				case *dns.SVCBIPv4Hint:
					for _, ip := range v.Hint {
						add(ip, ttl)
					}
				case *dns.SVCBIPv6Hint:
					for _, ip := range v.Hint {
						add(ip, ttl)
					}
				}
			}
		}
	}
	if len(addresses) > 256 {
		return nil
	}
	result := make([]association, 0, len(addresses))
	for ip, ttl := range addresses {
		result = append(result, association{
			Domain:  root,
			Address: ip,
			Expires: now.Add(time.Duration(max(ttl, 1)) * time.Second),
		})
	}
	return result
}

func (s *Service) ObserveDNSResponse(ctx context.Context, query, response *dns.Msg) {
	if !s.running.Load() {
		return
	}
	m := adapter.ContextFrom(ctx)
	if m == nil || m.Outbound != "" || !slices.Contains(s.config.options.DNSInbounds, m.Inbound) {
		return
	}
	observedAt := time.Now()
	warm := true
	defer func() {
		index := 0
		if warm {
			index = 1
		}
		s.dnsLatency[index].observe(time.Since(observedAt))
	}()
	values := responseAssociations(query, response, observedAt)
	if len(values) == 0 {
		return
	}
	lease := newDNSResponseLease(s.state, values, observedAt)
	queued := false
	defer func() {
		lease.finish(queued && ctx.Err() == nil && s.ctx.Err() == nil)
	}()
	now := time.Now()
	snap := s.snapshot.Load()
	for _, a := range values {
		if s.config.excludedIP(a.Address) {
			continue
		}
		if !s.state.known(a, now) {
			warm = false
			break
		}
		if s.config.options.Advertise && s.state.lookup(a.Address, now) != "" && !snap.Actual[netip.PrefixFrom(a.Address, a.Address.BitLen())] {
			warm = false
			break
		}
	}
	req := learnRequest{
		values:   values,
		done:     make(chan bool, 1),
		response: lease,
	}
	select {
	case s.requests <- req:
		queued = true
	default:
		s.stats.queueRejected.Add(1)
	}
	if !s.config.options.Advertise {
		return
	}
	waitForCommit := !warm && queued
	if !waitForCommit && !s.dnsSettleUntil(values).After(time.Now()) {
		return
	}
	warm = false
	start := time.Now()
	defer func() {
		s.stats.dnsWaitNanos.Add(uint64(time.Since(start)))
		s.stats.dnsWaitCount.Add(1)
	}()
	if waitForCommit {
		timer := time.NewTimer(time.Duration(s.config.options.Timing.CommitTimeout))
		defer timer.Stop()
		select {
		case <-req.done:
		case <-timer.C:
			s.stats.commitTimeout.Add(1)
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
	for {
		remaining := time.Until(s.dnsSettleUntil(values))
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (s *Service) dnsSettleUntil(values []association) time.Time {
	snapshot := s.snapshot.Load()
	var until time.Time
	for _, a := range values {
		if s.config.excludedIP(a.Address) {
			continue
		}
		if deadline := snapshot.SettleUntil[netip.PrefixFrom(a.Address, a.Address.BitLen())]; deadline.After(until) {
			until = deadline
		}
	}
	return until
}
