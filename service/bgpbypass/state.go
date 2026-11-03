package bgpbypass

import (
	"container/heap"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const shardCount = 64

type association struct {
	Domain     string     `json:"domain"`
	Address    netip.Addr `json:"address"`
	Expires    time.Time  `json:"expires"`
	choice     decision
	index      int
	pendingDNS int
}

func (a *association) valid(now time.Time) bool {
	return a.Expires.After(now) || a.pendingDNS > 0
}

type expiryQueue []*association

func (q expiryQueue) Len() int           { return len(q) }
func (q expiryQueue) Less(i, j int) bool { return q[i].Expires.Before(q[j].Expires) }
func (q expiryQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index = i
	q[j].index = j
}

func (q *expiryQueue) Push(v any) {
	a := v.(*association)
	a.index = len(*q)
	*q = append(*q, a)
}

func (q *expiryQueue) Pop() any {
	old := *q
	a := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	a.index = -1
	return a
}

type destination struct {
	leases     map[string]*association
	active     map[string]int
	withdrawAt time.Time
	wasWanted  bool
	routeSlot  bool
}

type stateShard struct {
	sync.RWMutex
	entries map[netip.Addr]*destination
	expiry  expiryQueue
}

type routeState struct {
	shards             [shardCount]stateShard
	conflicts          atomic.Int64
	count              atomic.Int64
	addresses          atomic.Int64
	routeSlots         atomic.Int64
	limitsRoutes       int
	limitsAssociations int
	withdrawDelay      time.Duration
	config             *compiledConfig
	fixedAccess        sync.RWMutex
	fixed              map[netip.Prefix]string
}

func newRouteState(c *compiledConfig) *routeState {
	s := &routeState{
		config:             c,
		limitsRoutes:       c.options.Limits.MaxDynamicRoutes,
		limitsAssociations: c.options.Limits.MaxDNSAssociations,
		withdrawDelay:      time.Duration(*c.options.Timing.WithdrawDelay),
		fixed:              map[netip.Prefix]string{},
	}
	for i := range s.shards {
		s.shards[i].entries = map[netip.Addr]*destination{}
	}
	return s
}

func (s *routeState) shard(ip netip.Addr) *stateShard {
	a := ip.As16()
	return &s.shards[(uint16(a[14])<<8|uint16(a[15]))%shardCount]
}

func better(a, b decision) bool {
	return a.Outbound != "" && (b.Outbound == "" || a.Priority < b.Priority)
}

func winner(e *destination, now time.Time) decision {
	d := decision{}
	for _, a := range e.leases {
		if a.valid(now) && better(a.choice, d) {
			d = a.choice
		}
	}
	return d
}

func (s *routeState) learn(a association, now time.Time) bool {
	return s.learnAssociation(a, now, false) != nil
}

// retain protects a DNS response whose TTL will start when it is released.
// Such a response may already have spent its original TTL in the queue.
func (s *routeState) learnAssociation(a association, now time.Time, retain bool) *association {
	if s.config.excludedIP(a.Address) {
		return nil
	}
	sh := s.shard(a.Address)
	sh.Lock()
	defer sh.Unlock()
	e := sh.entries[a.Address]
	a.choice = s.config.decide(a.Domain, a.Address)
	if e != nil {
		if old := e.leases[a.Domain]; old != nil {
			if !a.Expires.After(now) && !retain && old.pendingDNS == 0 {
				return nil
			}
			// Refreshing an existing association needs no extra capacity.
			// Keep its policy and TTL even while route admission is pending.
			if a.choice.Outbound != "" && !e.routeSlot && s.routeSlots.Load() < int64(s.limitsRoutes) {
				e.routeSlot = true
				s.routeSlots.Add(1)
			}
			if a.Expires.After(old.Expires) {
				old.Expires = a.Expires
			}
			old.choice = a.choice
			if retain {
				old.pendingDNS++
			}
			sh.updateExpiry(old)
			return old
		}
	}
	if !a.Expires.After(now) && !retain {
		return nil
	}
	if a.choice.Outbound != "" && (e == nil || !e.routeSlot) && s.routeSlots.Load() >= int64(s.limitsRoutes) {
		return nil
	}
	if e == nil {
		if s.count.Load() >= int64(s.limitsAssociations) {
			return nil
		}
		e = &destination{leases: map[string]*association{}, active: map[string]int{}}
		sh.entries[a.Address] = e
		s.addresses.Add(1)
	}
	// A bounded per-address fan-in also bounds work on the connection hot path.
	if s.count.Load() >= int64(s.limitsAssociations) || len(e.leases) >= 256 {
		return nil
	}
	if a.choice.Outbound != "" && !e.routeSlot {
		e.routeSlot = true
		s.routeSlots.Add(1)
	}
	a.pendingDNS = 0
	if retain {
		a.pendingDNS = 1
	}
	e.leases[a.Domain] = &a
	heap.Push(&sh.expiry, &a)
	s.count.Add(1)
	return &a
}

// An expired association with pending DNS responses is kept outside the heap
// until it is refreshed or its final response releases it.
func (sh *stateShard) updateExpiry(a *association) {
	if a.index < 0 {
		heap.Push(&sh.expiry, a)
	} else {
		heap.Fix(&sh.expiry, a.index)
	}
}

func (s *routeState) fixedOutbound(ip netip.Addr) string {
	s.fixedAccess.RLock()
	defer s.fixedAccess.RUnlock()
	for bits := ip.BitLen(); bits > 0; bits-- {
		if out := s.fixed[netip.PrefixFrom(ip, bits).Masked()]; out != "" {
			return out
		}
	}
	return ""
}

func (s *routeState) lookup(ip netip.Addr, now time.Time) string {
	if !ip.IsValid() {
		return ""
	}
	ip = ip.Unmap()
	sh := s.shard(ip)
	sh.RLock()
	var d decision
	if e := sh.entries[ip]; e != nil && e.routeSlot {
		d = winner(e, now)
	}
	sh.RUnlock()
	if d.Outbound != "" {
		return d.Outbound
	}
	return s.fixedOutbound(ip)
}

func (s *routeState) pin(ip netip.Addr, out string) func() {
	if !ip.IsValid() || out == "" {
		return nil
	}
	ip = ip.Unmap()
	sh := s.shard(ip)
	sh.Lock()
	e := sh.entries[ip]
	// Fixed routes do not expire and need no per-connection allocation.
	if e == nil || !e.routeSlot {
		sh.Unlock()
		return nil
	}
	release := pinDestination(sh, ip, e, out)
	sh.Unlock()
	return release
}

func (s *routeState) acquire(ip netip.Addr, out string) (func(), bool) {
	if !ip.IsValid() || out == "" {
		return nil, false
	}
	ip = ip.Unmap()
	sh := s.shard(ip)
	sh.Lock()
	defer sh.Unlock()
	if e := sh.entries[ip]; e != nil && e.routeSlot {
		if d := winner(e, time.Now()); d.Outbound != "" {
			if d.Outbound != out {
				return nil, false
			}
			return pinDestination(sh, ip, e, out), true
		}
	}
	return nil, s.fixedOutbound(ip) == out
}

// The caller holds sh.Lock while confirming the destination and adding its
// reference, so desired cannot remove it between these operations.
func pinDestination(sh *stateShard, ip netip.Addr, e *destination, out string) func() {
	e.active[out]++
	return sync.OnceFunc(func() {
		sh.Lock()
		defer sh.Unlock()
		if current := sh.entries[ip]; current == e {
			e.active[out]--
			if e.active[out] == 0 {
				delete(e.active, out)
			}
		}
	})
}

// Only the controller calls expire/recompute/desired/learn. Connection readers
// and DNS/connection reference releases access one shard and perform no control I/O.
func (s *routeState) expire(now time.Time) {
	for i := range s.shards {
		sh := &s.shards[i]
		sh.Lock()
		for len(sh.expiry) > 0 && !sh.expiry[0].Expires.After(now) {
			a := heap.Pop(&sh.expiry).(*association)
			if a.pendingDNS > 0 {
				continue
			}
			if e := sh.entries[a.Address]; e != nil {
				delete(e.leases, a.Domain)
				s.count.Add(-1)
			}
		}
		sh.Unlock()
	}
}

func (s *routeState) recompute() {
	for i := range s.shards {
		sh := &s.shards[i]
		sh.Lock()
		for _, e := range sh.entries {
			for _, a := range e.leases {
				// Admission must not erase policy decisions. desired retries
				// pending destinations after old slots become available.
				a.choice = s.config.decide(a.Domain, a.Address)
			}
		}
		sh.Unlock()
	}
}

func (s *routeState) desired(now time.Time) map[netip.Prefix]bool {
	result := map[netip.Prefix]bool{}
	var pending []netip.Addr
	var conflicts int64
	s.fixedAccess.RLock()
	for p := range s.fixed {
		result[p] = true
	}
	s.fixedAccess.RUnlock()
	for i := range s.shards {
		sh := &s.shards[i]
		sh.Lock()
		for ip, e := range sh.entries {
			d := winner(e, now)
			if d.Outbound != "" && !e.routeSlot {
				pending = append(pending, ip)
			}
			for _, a := range e.leases {
				if a.valid(now) && a.choice.Outbound != d.Outbound {
					conflicts++
					break
				}
			}
			wanted := e.routeSlot && d.Outbound != ""
			for _, n := range e.active {
				if n > 0 {
					wanted = true
				}
			}
			if wanted {
				e.wasWanted = true
				e.withdrawAt = time.Time{}
			} else if e.wasWanted && e.withdrawAt.IsZero() {
				e.withdrawAt = now.Add(s.withdrawDelay)
			}
			if wanted || (e.wasWanted && now.Before(e.withdrawAt)) {
				result[netip.PrefixFrom(ip, ip.BitLen())] = true
			}
			if !wanted && !e.withdrawAt.IsZero() && !now.Before(e.withdrawAt) {
				e.wasWanted = false
			}
			if e.routeSlot && d.Outbound == "" && len(e.active) == 0 && !e.wasWanted {
				e.routeSlot = false
				s.routeSlots.Add(-1)
			}
			if len(e.leases) == 0 && len(e.active) == 0 && !e.wasWanted {
				delete(sh.entries, ip)
				s.addresses.Add(-1)
			}
		}
		sh.Unlock()
	}
	// Reclaim all expired slots before admitting replacements, independently
	// of shard traversal order. Active references and withdrawal grace still
	// retain their slots until the first pass releases them.
	for _, ip := range pending {
		if s.routeSlots.Load() >= int64(s.limitsRoutes) {
			break
		}
		sh := s.shard(ip)
		sh.Lock()
		if e := sh.entries[ip]; e != nil && !e.routeSlot && winner(e, now).Outbound != "" {
			e.routeSlot = true
			s.routeSlots.Add(1)
			e.wasWanted = true
			e.withdrawAt = time.Time{}
			result[netip.PrefixFrom(ip, ip.BitLen())] = true
		}
		sh.Unlock()
	}
	s.conflicts.Store(conflicts)
	return result
}

func (s *routeState) snapshot(now time.Time) []association {
	var result []association
	for i := range s.shards {
		sh := &s.shards[i]
		sh.RLock()
		for _, e := range sh.entries {
			for _, a := range e.leases {
				if a.Expires.After(now) {
					result = append(result, *a)
				}
			}
		}
		sh.RUnlock()
	}
	return result
}

func (s *routeState) known(a association, now time.Time) bool {
	if !a.Address.IsValid() {
		return false
	}
	sh := s.shard(a.Address)
	sh.RLock()
	defer sh.RUnlock()
	e := sh.entries[a.Address]
	if e == nil {
		return false
	}
	old := e.leases[a.Domain]
	return old != nil && old.valid(now)
}
