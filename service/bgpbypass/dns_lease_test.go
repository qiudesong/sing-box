package bgpbypass

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func observeLeaseResponse(t *testing.T, s *Service, ctx context.Context, ttl uint32) <-chan struct{} {
	t.Helper()
	query := new(dns.Msg)
	query.SetQuestion("a.example.", dns.TypeA)
	response := new(dns.Msg)
	response.SetReply(query)
	answer := rr(t, "a.example. 1 IN A 8.8.8.8")
	answer.Header().Ttl = ttl
	response.Answer = []dns.RR{answer}
	done := make(chan struct{})
	s.wg.Go(func() {
		ctx := adapter.WithContext(ctx, &adapter.InboundContext{Inbound: "lan"})
		s.ObserveDNSResponse(ctx, query, response)
		close(done)
	})
	return done
}

func TestDNSLeaseCoversReturnedTTL(t *testing.T) {
	for _, delay := range []time.Duration{100 * time.Millisecond, 2 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testService(t)
				s.config.options.Advertise = true
				settle := badoption.Duration(delay)
				s.config.options.Timing.SettleDelay = &settle
				s.state.withdrawDelay = 0
				s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
				s.running.Store(true)
				defer func() {
					s.cancel()
					s.wg.Wait()
				}()
				s.wg.Go(s.run)
				done := observeLeaseResponse(t, s, context.Background(), 1)
				synctest.Wait()
				ip := netip.MustParseAddr("8.8.8.8")
				if delay > time.Second {
					time.Sleep(time.Second)
					synctest.Wait()
					require.Equal(t, "proxy", s.state.lookup(ip, time.Now()), "waiting responses must retain their matching association")
					require.True(t, s.snapshot.Load().Actual[netip.PrefixFrom(ip, 32)])
					time.Sleep(delay - time.Second)
				} else {
					time.Sleep(delay)
				}
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("DNS response did not finish after settle_delay")
				}
				time.Sleep(900 * time.Millisecond)
				synctest.Wait()
				require.Equal(t, "proxy", s.state.lookup(ip, time.Now()), "the returned TTL is still valid")
				require.True(t, s.snapshot.Load().Actual[netip.PrefixFrom(ip, 32)])
				time.Sleep(1100 * time.Millisecond)
				synctest.Wait()
				require.Empty(t, s.state.lookup(ip, time.Now()))
				require.Empty(t, s.snapshot.Load().Actual)
				require.Zero(t, s.state.count.Load(), "finished responses must not leak DNS references")
			})
		})
	}
}

func TestWarmDNSRefreshFinishesBeforeController(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testService(t)
		s.config.options.Advertise = true
		s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
		s.running.Store(true)
		defer func() {
			s.cancel()
			s.wg.Wait()
		}()
		ip := netip.MustParseAddr("8.8.8.8")
		processLearning(s, []association{{
			Domain:  "a.example",
			Address: ip,
			Expires: time.Now().Add(time.Second),
		}})
		time.Sleep(750 * time.Millisecond)
		returnedAt := time.Now()
		done := observeLeaseResponse(t, s, context.Background(), 10)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("warm response waited for the controller")
		}
		request := <-s.requests
		time.Sleep(500 * time.Millisecond)
		s.state.expire(time.Now())
		require.Equal(t, "proxy", s.state.lookup(ip, time.Now()), "warm TTL renewal must be synchronous")
		lease := s.state.shard(ip).entries[ip].leases["a.example"]
		require.Equal(t, returnedAt.Add(10*time.Second), lease.Expires)
		require.Zero(t, lease.pendingDNS)
		s.processRequests([]learnRequest{request})
		require.Equal(t, returnedAt.Add(10*time.Second), lease.Expires, "late queue processing must not restart TTL")
		require.Zero(t, lease.pendingDNS)
		require.Len(t, s.state.shard(ip).expiry, 1)
	})
}

func TestDNSLeaseHandoffAndCancellation(t *testing.T) {
	for _, order := range []string{"learn before return", "return before learn", "cancel before learn", "cancel after learn"} {
		t.Run(order, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state := newRouteState(testConfig(t))
				state.withdrawDelay = 0
				ip := netip.MustParseAddr("8.8.8.8")
				now := time.Now()
				response := newDNSResponseLease(state, []association{{
					Domain:  "a.example",
					Address: ip,
					Expires: now.Add(time.Second),
				}}, now)
				learnFirst := order == "learn before return" || order == "cancel after learn"
				delivered := order == "learn before return" || order == "return before learn"
				if learnFirst {
					dirty, rejected := response.learn(time.Now())
					require.True(t, dirty)
					require.Zero(t, rejected)
					// This other lease shares a shard. An expired pending
					// response must not block the rest of the expiry heap.
					require.True(t, state.learn(association{
						Domain:  "other.example",
						Address: netip.MustParseAddr("8.8.8.72"),
						Expires: now.Add(1500 * time.Millisecond),
					}, now))
				}
				time.Sleep(2 * time.Second)
				state.expire(time.Now())
				if learnFirst {
					require.Equal(t, "proxy", state.lookup(ip, time.Now()))
					require.EqualValues(t, 1, state.count.Load())
					require.Empty(t, state.shard(ip).expiry)
				}
				response.finish(delivered)
				response.finish(delivered) // Completion is idempotent.
				if !learnFirst {
					time.Sleep(250 * time.Millisecond)
					_, rejected := response.learn(time.Now())
					require.Zero(t, rejected)
				}
				state.expire(time.Now())
				if delivered {
					lease := state.shard(ip).entries[ip].leases["a.example"]
					require.Equal(t, now.Add(3*time.Second), lease.Expires)
					require.Zero(t, lease.pendingDNS)
					require.Len(t, state.shard(ip).expiry, 1)
					time.Sleep(time.Second)
					state.expire(time.Now())
				}
				state.desired(time.Now())
				require.Zero(t, state.count.Load())
				require.Zero(t, state.addresses.Load())
				require.Zero(t, state.routeSlots.Load())
			})
		})
	}
}

func TestConcurrentDNSResponsesDoNotShortenTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		state := newRouteState(testConfig(t))
		ip := netip.MustParseAddr("8.8.8.8")
		now := time.Now()
		response := func(ttl time.Duration) *dnsResponseLease {
			return newDNSResponseLease(state, []association{{
				Domain:  "a.example",
				Address: ip,
				Expires: now.Add(ttl),
			}}, now)
		}
		long := response(10 * time.Second)
		long.learn(now)
		short := response(time.Second)
		short.learn(now)
		time.Sleep(2 * time.Second)
		long.finish(true)
		short.finish(true)
		lease := state.shard(ip).entries[ip].leases["a.example"]
		require.Equal(t, time.Now().Add(10*time.Second), lease.Expires)
		require.Zero(t, lease.pendingDNS)
		// A late queued refresh and a shorter ordinary learn must preserve
		// the lease already promised to the longer-lived DNS response.
		short.learn(time.Now())
		require.True(t, state.learn(association{
			Domain:  "a.example",
			Address: ip,
			Expires: time.Now().Add(time.Second),
		}, time.Now()))
		require.Equal(t, time.Now().Add(10*time.Second), lease.Expires)
		require.Len(t, state.shard(ip).expiry, 1)
	})
}

func TestDNSLeaseConcurrentCompletionAndExpiry(t *testing.T) {
	config := testConfig(t)
	for range 100 {
		state := newRouteState(config)
		now := time.Now()
		ip := netip.MustParseAddr("8.8.8.8")
		value := association{
			Domain:  "a.example",
			Address: ip,
			Expires: now.Add(time.Second),
		}
		require.True(t, state.learn(value, now))
		response := newDNSResponseLease(state, []association{value}, now)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			response.learn(now)
		})
		wg.Go(func() {
			<-start
			response.finish(true)
		})
		wg.Go(func() {
			<-start
			state.expire(now.Add(2 * time.Second))
		})
		close(start)
		wg.Wait()
		state.expire(now.Add(2 * time.Second))
		require.Zero(t, state.count.Load())
		require.Empty(t, state.shard(ip).expiry)
	}
}

func TestWaitingDNSLeaseCancellation(t *testing.T) {
	for _, serviceCancellation := range []bool{false, true} {
		name := "request context"
		if serviceCancellation {
			name = "service context"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testService(t)
				s.config.options.Advertise = true
				settle := badoption.Duration(time.Hour)
				s.config.options.Timing.SettleDelay = &settle
				s.state.withdrawDelay = 0
				s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
				s.running.Store(true)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				defer func() {
					s.cancel()
					s.wg.Wait()
				}()
				s.wg.Go(s.run)
				done := observeLeaseResponse(t, s, ctx, 1)
				synctest.Wait()
				time.Sleep(2 * time.Second)
				synctest.Wait()
				require.EqualValues(t, 1, s.state.count.Load())
				if serviceCancellation {
					s.cancel()
				} else {
					cancel()
				}
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("cancellation did not release the DNS response")
				}
				s.state.expire(time.Now())
				s.reconcile(time.Now())
				require.Zero(t, s.state.count.Load(), "cancellation must release expired pending leases")
				require.Empty(t, s.snapshot.Load().Actual)
			})
		})
	}
}

func TestRejectedDNSRefreshDoesNotRenewLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testService(t)
		s.config.options.Advertise = true
		s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
		s.running.Store(true)
		defer func() {
			s.cancel()
			s.wg.Wait()
		}()
		ip := netip.MustParseAddr("8.8.8.8")
		expires := time.Now().Add(time.Second)
		processLearning(s, []association{{
			Domain:  "a.example",
			Address: ip,
			Expires: expires,
		}})
		s.requests <- learnRequest{}
		done := observeLeaseResponse(t, s, context.Background(), 60)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("rejected refresh did not return")
		}
		lease := s.state.shard(ip).entries[ip].leases["a.example"]
		require.Equal(t, expires, lease.Expires)
		require.Zero(t, lease.pendingDNS)
		require.EqualValues(t, 1, s.stats.queueRejected.Load())
		s.state.expire(time.Now().Add(2 * time.Second))
		require.Zero(t, s.state.count.Load())
	})
}
