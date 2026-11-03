package bgpbypass

import (
	"context"
	"maps"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sagernet/sing-box/adapter"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

type retryBackend struct {
	partialBackend
	failAdd     bool
	omitAdd     bool
	failListAt  int
	failDelete  bool
	listCalls   int
	addCalls    int
	deleteCalls int
	beforeAdd   func()
}

func (b *retryBackend) List() (map[netip.Prefix]bool, error) {
	b.listCalls++
	if b.listCalls == b.failListAt {
		return nil, E.New("temporary RIB read failure")
	}
	return b.partialBackend.List()
}

func (b *retryBackend) Add(prefixes []netip.Prefix) error {
	b.addCalls++
	if b.failAdd {
		b.failAdd = false
		return E.New("temporary BGP add failure")
	}
	if b.omitAdd {
		b.omitAdd = false
		return nil
	}
	if b.beforeAdd != nil {
		b.beforeAdd()
	}
	return b.partialBackend.Add(prefixes)
}

func (b *retryBackend) Delete(prefixes []netip.Prefix) error {
	b.deleteCalls++
	if b.failDelete {
		b.failDelete = false
		return E.New("temporary BGP delete failure")
	}
	return b.partialBackend.Delete(prefixes)
}

func processLearning(s *Service, values []association) bool {
	done := make(chan bool, 1)
	s.processRequests([]learnRequest{{values: values, done: done}})
	return <-done
}

func TestKnownLeaseRetriesUncommittedRoutes(t *testing.T) {
	for _, failure := range []string{"add", "partial add", "missing route", "initial read", "readback", "delete"} {
		t.Run(failure, func(t *testing.T) {
			s := testService(t)
			s.config.options.Advertise = true
			backend := &retryBackend{
				partialBackend: partialBackend{routes: map[netip.Prefix]bool{}},
			}
			s.backend = backend
			now := time.Now()
			values := []association{{
				Domain:  "a.example",
				Address: netip.MustParseAddr("8.8.8.8"),
				Expires: now.Add(time.Hour),
			}}
			switch failure {
			case "add":
				backend.failAdd = true
			case "partial add":
				backend.fail = true
				values = append(values, association{
					Domain:  "a.example",
					Address: netip.MustParseAddr("2606:4700:4700::1111"),
					Expires: now.Add(time.Hour),
				})
			case "missing route":
				backend.omitAdd = true
			case "initial read":
				backend.failListAt = 1
			case "readback":
				backend.failListAt = 2
			case "delete":
				backend.failDelete = true
				backend.routes[netip.MustParsePrefix("9.9.9.9/32")] = true
			}
			processLearning(s, values)
			for _, value := range values {
				require.True(t, s.state.known(value, now))
			}
			if failure == "missing route" {
				require.Empty(t, s.snapshot.Load().LastError, "RIB divergence also needs retry without an API error")
			} else {
				require.NotEmpty(t, s.snapshot.Load().LastError)
			}
			changed := processLearning(s, values)
			if failure == "readback" {
				require.False(t, changed, "a successful Add must not be repeated after readback fails")
				require.Equal(t, 1, backend.addCalls)
			} else {
				require.True(t, changed, "the known lease must retry before acknowledging the request")
			}
			snapshot := s.snapshot.Load()
			require.Empty(t, snapshot.LastError)
			require.Equal(t, snapshot.Desired, snapshot.Actual)
			require.Len(t, snapshot.Actual, len(values))
			// A converged warm refresh must not perform another full RIB scan.
			listCalls, addCalls, deleteCalls := backend.listCalls, backend.addCalls, backend.deleteCalls
			require.False(t, processLearning(s, values))
			require.Equal(t, listCalls, backend.listCalls)
			require.Equal(t, addCalls, backend.addCalls)
			require.Equal(t, deleteCalls, backend.deleteCalls)
		})
	}
}

func TestKnownLeaseObservationModeSkipsReconciliation(t *testing.T) {
	s := testService(t)
	now := time.Now()
	values := []association{{
		Domain:  "a.example",
		Address: netip.MustParseAddr("8.8.8.8"),
		Expires: now.Add(time.Hour),
	}}
	require.False(t, processLearning(s, values))
	snapshot := s.snapshot.Load()
	require.Len(t, snapshot.Desired, 1)
	require.Empty(t, snapshot.Actual)
	require.False(t, processLearning(s, values))
	require.Same(t, snapshot, s.snapshot.Load())
}

func TestDNSResponseWaitsForKnownLeaseRetry(t *testing.T) {
	s := testService(t)
	s.config.options.Advertise = true
	s.config.options.Timing.CommitTimeout = badoption.Duration(5 * time.Second)
	s.running.Store(true)
	backend := &retryBackend{
		partialBackend: partialBackend{routes: map[netip.Prefix]bool{}},
		failAdd:        true,
	}
	s.backend = backend
	ip := netip.MustParseAddr("8.8.8.8")
	processLearning(s, []association{{
		Domain:  "a.example",
		Address: ip,
		Expires: time.Now().Add(time.Hour),
	}})
	require.Empty(t, s.snapshot.Load().Actual)
	started := make(chan struct{})
	allowCommit := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(allowCommit) })
	backend.beforeAdd = func() {
		close(started)
		<-allowCommit
	}
	// Drive the real request handler without the periodic reconciliation timer,
	// so only this DNS request can repair the failed submission.
	s.wg.Go(func() {
		select {
		case request := <-s.requests:
			s.processRequests([]learnRequest{request})
		case <-s.ctx.Done():
		}
	})
	t.Cleanup(func() {
		unblock()
		s.cancel()
		s.wg.Wait()
	})
	query := new(dns.Msg)
	query.SetQuestion("a.example.", dns.TypeA)
	response := new(dns.Msg)
	response.SetReply(query)
	response.Answer = []dns.RR{rr(t, "a.example. 60 IN A 8.8.8.8")}
	finished := make(chan struct{})
	s.wg.Go(func() {
		ctx := adapter.WithContext(context.Background(), &adapter.InboundContext{Inbound: "lan"})
		s.ObserveDNSResponse(ctx, query, response)
		close(finished)
	})
	select {
	case <-started:
	case <-finished:
		t.Fatal("DNS response returned without retrying the missing route")
	case <-time.After(3 * time.Second):
		t.Fatal("DNS request did not retry the missing route")
	}
	select {
	case <-finished:
		t.Fatal("DNS response returned before the retry committed")
	default:
	}
	unblock()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("DNS response did not resume after the retry committed")
	}
	require.True(t, s.snapshot.Load().Actual[netip.PrefixFrom(ip, 32)])
	require.EqualValues(t, 1, s.stats.dnsWaitCount.Load())
}

func TestConcurrentDNSResponsesShareSettleWindow(t *testing.T) {
	for _, name := range []string{"queued refresh", "rejected refresh", "commit timeout"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testService(t)
				s.config.options.Advertise = true
				settle := badoption.Duration(time.Second)
				s.config.options.Timing.SettleDelay = &settle
				s.config.options.Timing.CommitTimeout = badoption.Duration(100 * time.Millisecond)
				s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
				s.running.Store(true)
				defer func() {
					s.cancel()
					s.wg.Wait()
				}()
				observe := func(domain string) <-chan struct{} {
					query := new(dns.Msg)
					query.SetQuestion(domain, dns.TypeA)
					response := new(dns.Msg)
					response.SetReply(query)
					response.Answer = []dns.RR{rr(t, domain+" 60 IN A 8.8.8.8")}
					done := make(chan struct{})
					s.wg.Go(func() {
						ctx := adapter.WithContext(context.Background(), &adapter.InboundContext{Inbound: "lan"})
						s.ObserveDNSResponse(ctx, query, response)
						close(done)
					})
					return done
				}
				first := observe("a.example.")
				s.processRequests([]learnRequest{<-s.requests})
				synctest.Wait()
				require.True(t, s.snapshot.Load().Actual[netip.MustParsePrefix("8.8.8.8/32")])
				select {
				case <-first:
					t.Fatal("first response returned before settle_delay")
				default:
				}
				time.Sleep(250 * time.Millisecond)
				if name == "rejected refresh" {
					s.requests <- learnRequest{}
				}
				domain := "a.example."
				if name == "commit timeout" {
					domain = "b.example."
				}
				second := observe(domain)
				synctest.Wait()
				select {
				case <-second:
					t.Fatal("concurrent response bypassed the shared settle window")
				default:
				}
				// Both requests finish at the original deadline. A refresh does
				// not start a new full settle_delay or wait for another commit.
				time.Sleep(750 * time.Millisecond)
				synctest.Wait()
				for _, done := range []<-chan struct{}{first, second} {
					select {
					case <-done:
					default:
						t.Fatal("response did not finish at the shared deadline")
					}
				}
				if name == "commit timeout" {
					require.EqualValues(t, 1, s.stats.commitTimeout.Load())
				}
			})
		})
	}
}

func TestReconcilePreservesSettleWindows(t *testing.T) {
	for _, failure := range []string{"none", "readback", "initial read", "partial add"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testService(t)
				s.config.options.Advertise = true
				settle := badoption.Duration(time.Second)
				s.config.options.Timing.SettleDelay = &settle
				backend := &retryBackend{
					partialBackend: partialBackend{routes: map[netip.Prefix]bool{}},
				}
				s.backend = backend
				if failure == "readback" {
					backend.failListAt = 2
				} else if failure == "partial add" {
					backend.fail = true
				}
				values := []association{{
					Domain:  "a.example",
					Address: netip.MustParseAddr("8.8.8.8"),
					Expires: time.Now().Add(time.Hour),
				}, {
					Domain:  "a.example",
					Address: netip.MustParseAddr("1.1.1.1"),
					Expires: time.Now().Add(time.Hour),
				}}
				processLearning(s, values)
				original := s.snapshot.Load()
				deadlines := maps.Clone(original.SettleUntil)
				require.Len(t, deadlines, len(backend.routes))
				for _, until := range deadlines {
					require.Equal(t, time.Now().Add(time.Second), until)
				}
				time.Sleep(250 * time.Millisecond)
				if failure == "initial read" {
					backend.failListAt = backend.listCalls + 1
				}
				s.reconcile(time.Now())
				for p, until := range deadlines {
					require.Equal(t, until, s.snapshot.Load().SettleUntil[p], "reconciliation must not restart the existing window")
				}
				s.reconcile(time.Now())
				require.Equal(t, deadlines, original.SettleUntil, "published snapshots must remain immutable")
				if failure == "partial add" {
					require.Len(t, s.snapshot.Load().SettleUntil, 2, "the retried prefix has its own deadline")
				}
				time.Sleep(time.Second)
				s.reconcile(time.Now())
				require.Empty(t, s.snapshot.Load().SettleUntil, "completed windows must be reclaimed")
			})
		})
	}
}

func TestReannouncedRouteStartsNewSettleWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := testService(t)
		settle := badoption.Duration(time.Second)
		s.config.options.Timing.SettleDelay = &settle
		backend := &partialBackend{routes: map[netip.Prefix]bool{}}
		s.backend = backend
		ip := netip.MustParseAddr("8.8.8.8")
		prefix := netip.PrefixFrom(ip, 32)
		processLearning(s, []association{{
			Domain:  "a.example",
			Address: ip,
			Expires: time.Now().Add(time.Hour),
		}})
		until := s.snapshot.Load().SettleUntil[prefix]
		time.Sleep(250 * time.Millisecond)
		delete(backend.routes, prefix)
		s.reconcile(time.Now())
		require.Equal(t, until.Add(250*time.Millisecond), s.snapshot.Load().SettleUntil[prefix])
	})
}

func TestDNSResponseWaitsOnlyForItsPrefixes(t *testing.T) {
	for _, multipleAddresses := range []bool{false, true} {
		name := "single prefix"
		if multipleAddresses {
			name = "latest prefix deadline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testService(t)
				s.config.options.Advertise = true
				settle := badoption.Duration(time.Second)
				s.config.options.Timing.SettleDelay = &settle
				s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
				s.running.Store(true)
				defer func() {
					s.cancel()
					s.wg.Wait()
				}()
				for _, ip := range []string{"8.8.8.8", "1.1.1.1"} {
					processLearning(s, []association{{
						Domain:  "a.example",
						Address: netip.MustParseAddr(ip),
						Expires: time.Now().Add(time.Hour),
					}})
					time.Sleep(250 * time.Millisecond)
				}
				query := new(dns.Msg)
				query.SetQuestion("a.example.", dns.TypeA)
				response := new(dns.Msg)
				response.SetReply(query)
				response.Answer = []dns.RR{rr(t, "a.example. 60 IN A 8.8.8.8")}
				if multipleAddresses {
					response.Answer = append(response.Answer, rr(t, "a.example. 60 IN A 1.1.1.1"))
				}
				done := make(chan struct{})
				s.wg.Go(func() {
					ctx := adapter.WithContext(context.Background(), &adapter.InboundContext{Inbound: "lan"})
					s.ObserveDNSResponse(ctx, query, response)
					close(done)
				})
				synctest.Wait()
				time.Sleep(500 * time.Millisecond)
				synctest.Wait()
				if multipleAddresses {
					select {
					case <-done:
						t.Fatal("response returned before its last prefix settled")
					default:
					}
					time.Sleep(250 * time.Millisecond)
					synctest.Wait()
				}
				select {
				case <-done:
				default:
					t.Fatal("response waited beyond its prefixes' deadlines")
				}
			})
		})
	}
}

func TestDNSSettleWaitCancellation(t *testing.T) {
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
				s.backend = &partialBackend{routes: map[netip.Prefix]bool{}}
				s.running.Store(true)
				processLearning(s, []association{{
					Domain:  "a.example",
					Address: netip.MustParseAddr("8.8.8.8"),
					Expires: time.Now().Add(time.Hour),
				}})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				defer func() {
					s.cancel()
					s.wg.Wait()
				}()
				query := new(dns.Msg)
				query.SetQuestion("a.example.", dns.TypeA)
				response := new(dns.Msg)
				response.SetReply(query)
				response.Answer = []dns.RR{rr(t, "a.example. 60 IN A 8.8.8.8")}
				done := make(chan struct{})
				s.wg.Go(func() {
					ctx := adapter.WithContext(ctx, &adapter.InboundContext{Inbound: "lan"})
					s.ObserveDNSResponse(ctx, query, response)
					close(done)
				})
				synctest.Wait()
				select {
				case <-done:
					t.Fatal("response bypassed the pending settle window")
				default:
				}
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
			})
		})
	}
}
