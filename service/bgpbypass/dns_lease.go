package bgpbypass

import (
	"net/netip"
	"sync"
	"time"
)

type dnsLeaseReference struct {
	association *association
	ttl         time.Duration
}

// A response can finish before the controller consumes its learning request.
// Serialize that handoff with completion, without holding a lock during BGP I/O.
type dnsResponseLease struct {
	access     sync.Mutex
	state      *routeState
	values     []association
	observedAt time.Time
	held       map[netip.Addr]dnsLeaseReference
	finished   bool
	delivered  bool
	returnedAt time.Time
}

func newDNSResponseLease(state *routeState, values []association, now time.Time) *dnsResponseLease {
	r := &dnsResponseLease{
		state:      state,
		values:     values,
		observedAt: now,
		held:       map[netip.Addr]dnsLeaseReference{},
	}
	// Protect warm associations before the response can return or the next
	// controller sweep can expire them. New entries still require admission.
	for _, a := range values {
		if state.config.excludedIP(a.Address) {
			continue
		}
		sh := state.shard(a.Address)
		sh.Lock()
		if e := sh.entries[a.Address]; e != nil {
			if old := e.leases[a.Domain]; old != nil && old.valid(now) {
				old.pendingDNS++
				r.held[a.Address] = dnsLeaseReference{
					association: old,
					ttl:         a.Expires.Sub(now),
				}
			}
		}
		sh.Unlock()
	}
	return r
}

func (r *dnsResponseLease) learn(now time.Time) (dirty bool, rejected uint64) {
	r.access.Lock()
	defer r.access.Unlock()
	if r.finished && !r.delivered {
		return
	}
	for _, a := range r.values {
		ttl := a.Expires.Sub(r.observedAt)
		if r.finished {
			a.Expires = r.returnedAt.Add(ttl)
			if !a.Expires.After(now) {
				continue
			}
		}
		if r.state.config.excludedIP(a.Address) {
			continue
		}
		if !r.state.known(a, now) {
			dirty = true
		}
		retain := !r.finished && r.held[a.Address].association == nil
		learned := r.state.learnAssociation(a, now, retain)
		if learned == nil {
			rejected++
			continue
		}
		if retain {
			r.held[a.Address] = dnsLeaseReference{
				association: learned,
				ttl:         ttl,
			}
		}
	}
	return
}

func (r *dnsResponseLease) finish(delivered bool) {
	r.access.Lock()
	defer r.access.Unlock()
	if r.finished {
		return
	}
	r.finished = true
	r.delivered = delivered
	r.returnedAt = time.Now()
	for _, held := range r.held {
		a := held.association
		sh := r.state.shard(a.Address)
		sh.Lock()
		if delivered {
			if expires := r.returnedAt.Add(held.ttl); expires.After(a.Expires) {
				a.Expires = expires
			}
		}
		a.pendingDNS--
		sh.updateExpiry(a)
		sh.Unlock()
	}
	clear(r.held)
}
