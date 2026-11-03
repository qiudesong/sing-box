package bgpbypass

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing-box/adapter"
	N "github.com/sagernet/sing/common/network"
)

func (s *Service) startHTTP(scope *adapter.Scope) error {
	o := s.config.options.Observability
	start := func(address string, port uint16, handler http.Handler) error {
		listener, err := net.Listen(N.NetworkTCP, net.JoinHostPort(address, strconv.Itoa(int(port))))
		if err != nil {
			return err
		}
		server := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    8192,
		}
		scope.Add(server.Close)
		go func() {
			if e := server.Serve(listener); e != nil && !errors.Is(e, http.ErrServerClosed) {
				s.logger.Error("observability: ", e)
			}
		}()
		return nil
	}
	if o.ListenPort != 0 {
		if err := start(o.Listen, o.ListenPort, s.handler(false)); err != nil {
			return err
		}
	}
	if o.HealthPort != 0 {
		return start(o.HealthListen, o.HealthPort, s.handler(true))
	}
	return nil
}

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

func (s *Service) handler(healthOnly bool) http.Handler {
	mux := http.NewServeMux()
	check := func(predicate func() bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if !predicate() {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprintln(w, "ok")
		}
	}
	mux.HandleFunc("/livez", check(s.running.Load))
	mux.HandleFunc("/readyz", check(s.ready))
	mux.HandleFunc("/dns-readyz", check(func() bool { return s.running.Load() && s.dnsReady.Load() }))
	if !healthOnly {
		if s.config.options.Observability.Metrics {
			mux.HandleFunc("/metrics", s.metrics)
		}
		if s.config.options.Observability.Diagnostics {
			mux.HandleFunc("/routes", s.routes)
			mux.HandleFunc("/routes/", s.explain)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		secret := s.config.options.Observability.Secret
		if !healthOnly && secret != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Service) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	snap := s.snapshot.Load()
	metric := func(name, kind string, value any) {
		fmt.Fprintf(w, "# TYPE sing_box_bgp_bypass_%s %s\nsing_box_bgp_bypass_%s{service=%q} %v\n", name, kind, name, s.Tag(), value)
	}
	metric("desired_routes", "gauge", len(snap.Desired))
	metric("actual_routes", "gauge", len(snap.Actual))
	metric("dns_associations", "gauge", s.state.count.Load())
	metric("pending_updates", "gauge", len(s.requests))
	metric("conflicting_addresses", "gauge", s.state.conflicts.Load())
	metric("queue_rejected_total", "counter", s.stats.queueRejected.Load())
	metric("capacity_rejected_total", "counter", s.stats.capacityRejected.Load())
	metric("commit_timeout_total", "counter", s.stats.commitTimeout.Load())
	metric("bgp_errors_total", "counter", s.stats.bgpErrors.Load())
	metric("reconciliations_total", "counter", s.stats.reconciliations.Load())
	metric("announced_total", "counter", s.stats.announced.Load())
	metric("withdrawn_total", "counter", s.stats.withdrawn.Load())
	metric("dns_wait_seconds_sum", "counter", float64(s.stats.dnsWaitNanos.Load())/1e9)
	metric("dns_wait_seconds_count", "counter", s.stats.dnsWaitCount.Load())
	fmt.Fprintln(w, "# TYPE sing_box_bgp_bypass_dns_observe_seconds histogram")
	for i, path := range []string{"cold", "warm"} {
		h := &s.dnsLatency[i]
		var count uint64
		for j := range h.buckets {
			count += h.buckets[j].Load()
			bound := "+Inf"
			if j < len(dnsLatencyBounds) {
				bound = strconv.FormatFloat(dnsLatencyBounds[j].Seconds(), 'g', -1, 64)
			}
			fmt.Fprintf(w, "sing_box_bgp_bypass_dns_observe_seconds_bucket{service=%q,reason=%q,le=%q} %d\n", s.Tag(), path, bound, count)
		}
		fmt.Fprintf(w, "sing_box_bgp_bypass_dns_observe_seconds_sum{service=%q,reason=%q} %g\nsing_box_bgp_bypass_dns_observe_seconds_count{service=%q,reason=%q} %d\n", s.Tag(), path, float64(h.nanos.Load())/1e9, s.Tag(), path, count)
	}
	for _, p := range s.config.options.BGP.Peers {
		value := 0
		if snap.Peers[p.Tag] {
			value = 1
		}
		for _, family := range p.Families {
			fmt.Fprintf(w, "sing_box_bgp_bypass_peer_up{service=%q,peer=%q,family=%q} %d\n", s.Tag(), p.Tag, family, value)
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
	ip, err := netip.ParseAddr(strings.TrimPrefix(r.URL.Path, "/routes/"))
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
