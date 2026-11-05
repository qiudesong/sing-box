package bgpbypass

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
)

func (s *Service) ready() bool {
	if !s.running.Load() || !s.configValid.Load() || !s.dnsReady.Load() {
		return false
	}
	if !s.config.options.Advertise {
		return true
	}
	snap := s.snapshot.Load()
	if snap.LastError != "" || time.Since(snap.At) > 5*time.Second {
		return false
	}
	for _, p := range s.config.options.BGP.Peers {
		if !snap.Peers[p.Tag] {
			return false
		}
	}
	return true
}

func (s *Service) HealthChecks() map[string]func() bool {
	return map[string]func() bool{
		"livez":      s.running.Load,
		"readyz":     s.ready,
		"dns-readyz": func() bool { return s.running.Load() && s.dnsReady.Load() },
	}
}

func (s *Service) RegisterDiagnostics(mux *http.ServeMux) {
	mux.HandleFunc("/routes", s.routes)
	mux.HandleFunc("/routes/{ip}", s.explain)
}

func (s *Service) CollectMetrics(emit func(adapter.Metric), _ []string) {
	snap := s.snapshot.Load()
	metric := func(name string, kind adapter.MetricType, value float64) {
		emit(adapter.Metric{
			Name:   C.MetricNamePrefixBGPBypass + name,
			Type:   kind,
			Value:  value,
			Labels: map[string]string{C.MetricLabelService: s.Tag()},
		})
	}
	metric("desired_routes", adapter.MetricGauge, float64(len(snap.Desired)))
	metric("actual_routes", adapter.MetricGauge, float64(len(snap.Actual)))
	metric("dns_associations", adapter.MetricGauge, float64(s.state.count.Load()))
	metric("pending_updates", adapter.MetricGauge, float64(len(s.requests)))
	metric("conflicting_addresses", adapter.MetricGauge, float64(s.state.conflicts.Load()))
	metric("queue_rejected_total", adapter.MetricCounter, float64(s.stats.queueRejected.Load()))
	metric("capacity_rejected_total", adapter.MetricCounter, float64(s.stats.capacityRejected.Load()))
	metric("commit_timeout_total", adapter.MetricCounter, float64(s.stats.commitTimeout.Load()))
	metric("bgp_errors_total", adapter.MetricCounter, float64(s.stats.bgpErrors.Load()))
	metric("reconciliations_total", adapter.MetricCounter, float64(s.stats.reconciliations.Load()))
	metric("announced_total", adapter.MetricCounter, float64(s.stats.announced.Load()))
	metric("withdrawn_total", adapter.MetricCounter, float64(s.stats.withdrawn.Load()))
	metric("dns_wait_seconds_sum", adapter.MetricCounter, float64(s.stats.dnsWaitNanos.Load())/1e9)
	metric("dns_wait_seconds_count", adapter.MetricCounter, float64(s.stats.dnsWaitCount.Load()))
	for i, reason := range []string{"cold", "warm"} {
		histogram := &s.dnsLatency[i]
		buckets := make(map[float64]uint64, len(dnsLatencyBounds))
		var count uint64
		for j := range histogram.buckets {
			count += histogram.buckets[j].Load()
			if j < len(dnsLatencyBounds) {
				buckets[dnsLatencyBounds[j].Seconds()] = count
			}
		}
		emit(adapter.Metric{
			Name:    C.MetricNameBGPBypassDNSObserve,
			Type:    adapter.MetricHistogram,
			Labels:  map[string]string{C.MetricLabelService: s.Tag(), C.MetricLabelReason: reason},
			Count:   count,
			Sum:     float64(histogram.nanos.Load()) / 1e9,
			Buckets: buckets,
		})
	}
	for _, peer := range s.config.options.BGP.Peers {
		var value float64
		if snap.Peers[peer.Tag] {
			value = 1
		}
		for _, family := range peer.Families {
			emit(adapter.Metric{
				Name:   C.MetricNameBGPBypassPeerUp,
				Type:   adapter.MetricGauge,
				Labels: map[string]string{C.MetricLabelService: s.Tag(), C.MetricLabelPeer: peer.Tag, C.MetricLabelFamily: family},
				Value:  value,
			})
		}
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Service) routes(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			http.Error(w, "limit must be 1..500", http.StatusBadRequest)
			return
		}
		limit = n
	}
	snap := s.snapshot.Load()
	all := map[string]bool{}
	for p := range snap.Desired {
		all[p.String()] = true
	}
	for p := range snap.Actual {
		all[p.String()] = true
	}
	keys := make([]string, 0, len(all))
	for p := range all {
		if p > r.URL.Query().Get("after") {
			keys = append(keys, p)
		}
	}
	slices.Sort(keys)
	next := ""
	if len(keys) > limit {
		keys = keys[:limit]
		next = keys[len(keys)-1]
	}
	type row struct {
		Prefix   string `json:"prefix"`
		Desired  bool   `json:"desired"`
		LocalRIB bool   `json:"local_rib"`
	}
	rows := make([]row, 0, len(keys))
	for _, key := range keys {
		p := netip.MustParsePrefix(key)
		rows = append(rows, row{key, snap.Desired[p], snap.Actual[p]})
	}
	writeJSON(w, map[string]any{
		"routes":                      rows,
		"next":                        next,
		"observed_at":                 snap.At,
		"last_error":                  snap.LastError,
		"advertise":                   s.config.options.Advertise,
		"dns_preflight_skipped_rules": s.config.skipped,
		"router_fib_confirmed":        false,
	})
}

func (s *Service) explain(w http.ResponseWriter, r *http.Request) {
	ip, err := netip.ParseAddr(r.PathValue("ip"))
	if err != nil {
		http.Error(w, "invalid IP", http.StatusBadRequest)
		return
	}
	ip = ip.Unmap()
	sh := s.state.shard(ip)
	sh.RLock()
	type lease struct {
		Domain     string    `json:"domain"`
		Expires    time.Time `json:"expires"`
		Outbound   string    `json:"outbound"`
		Priority   int       `json:"priority"`
		PendingDNS int       `json:"pending_dns"`
	}
	var leases []lease
	active := map[string]int{}
	now := time.Now()
	if e := sh.entries[ip]; e != nil {
		for _, a := range e.leases {
			if a.valid(now) {
				leases = append(leases, lease{
					Domain:     a.Domain,
					Expires:    a.Expires,
					Outbound:   a.choice.Outbound,
					Priority:   a.choice.Priority,
					PendingDNS: a.pendingDNS,
				})
			}
		}
		maps.Copy(active, e.active)
	}
	sh.RUnlock()
	slices.SortFunc(leases, func(a, b lease) int { return strings.Compare(a.Domain, b.Domain) })
	snap := s.snapshot.Load()
	p := netip.PrefixFrom(ip, ip.BitLen())
	writeJSON(w, map[string]any{
		"address":              ip,
		"leases":               leases,
		"active":               active,
		"active_outbounds":     active,
		"outbound":             s.state.lookup(ip, now),
		"fixed_outbound":       s.state.fixedOutbound(ip),
		"dynamic_desired":      snap.Desired[p],
		"dynamic_local_rib":    snap.Actual[p],
		"selection":            "proxy first, then lowest native route rule index; direct does not override a persistent route",
		"router_fib_confirmed": false,
	})
}
