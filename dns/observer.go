package dns

import (
	"context"
	"sync"

	"github.com/sagernet/sing-box/adapter"

	mDNS "github.com/miekg/dns"
)

type (
	responseObserver  struct{ observer adapter.DNSResponseObserver }
	responseObservers struct{ values []*responseObserver }
)

func (r *Router) RegisterDNSResponseObserver(observer adapter.DNSResponseObserver) func() {
	r.observerAccess.Lock()
	var values []*responseObserver
	if old := r.observers.Load(); old != nil {
		values = append(values, old.values...)
	}
	entry := &responseObserver{observer}
	values = append(values, entry)
	r.observers.Store(&responseObservers{values})
	r.observerAccess.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.observerAccess.Lock()
			defer r.observerAccess.Unlock()
			var values []*responseObserver
			for _, item := range r.observers.Load().values {
				if item != entry {
					values = append(values, item)
				}
			}
			r.observers.Store(&responseObservers{values})
		})
	}
}

func (r *Router) DNSCacheExpires() bool { return !r.disableExpire }

func (r *Router) observeResponse(ctx context.Context, query, response *mDNS.Msg, err error) {
	if err != nil || response == nil {
		return
	}
	if observers := r.observers.Load(); observers != nil {
		for _, observer := range observers.values {
			observer.observer.ObserveDNSResponse(ctx, query, response)
		}
	}
}
