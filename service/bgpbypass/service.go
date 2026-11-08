package bgpbypass

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

type learnRequest struct {
	values   []association
	done     chan bool
	response *dnsResponseLease
}

type counters struct {
	queueRejected    atomic.Uint64
	capacityRejected atomic.Uint64
	commitTimeout    atomic.Uint64
	bgpErrors        atomic.Uint64
	reconciliations  atomic.Uint64
	dnsWaitNanos     atomic.Uint64
	dnsWaitCount     atomic.Uint64
	announced        atomic.Uint64
	withdrawn        atomic.Uint64
}

var dnsLatencyBounds = [...]time.Duration{
	100 * time.Microsecond,
	500 * time.Microsecond,
	time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
}

type latencyHistogram struct {
	buckets [len(dnsLatencyBounds) + 1]atomic.Uint64
	nanos   atomic.Uint64
}

func (h *latencyHistogram) observe(d time.Duration) {
	i := 0
	for i < len(dnsLatencyBounds) && d > dnsLatencyBounds[i] {
		i++
	}
	h.buckets[i].Add(1)
	h.nanos.Add(uint64(d))
}

type controlSnapshot struct {
	Desired     map[netip.Prefix]bool
	Actual      map[netip.Prefix]bool
	SettleUntil map[netip.Prefix]time.Time
	Peers       map[string]bool
	LastError   string
	At          time.Time
	// Cache convergence here so warm DNS refreshes do not compare the full RIB.
	Converged bool
}

type Service struct {
	boxService.Adapter
	ctx         context.Context
	cancel      context.CancelFunc
	logger      log.ContextLogger
	router      adapter.Router
	dns         adapter.DNSRouter
	outbounds   adapter.OutboundManager
	config      *compiledConfig
	state       *routeState
	backend     routeBackend
	requests    chan learnRequest
	updates     chan struct{}
	wg          sync.WaitGroup
	running     atomic.Bool
	configValid atomic.Bool
	closed      atomic.Bool
	snapshot    atomic.Pointer[controlSnapshot]
	stats       counters
	dnsLatency  [2]latencyHistogram // cold, warm; no domain or address labels
	dnsReady    atomic.Bool
}

func RegisterService(registry *boxService.Registry) {
	boxService.Register[option.BGPBypassServiceOptions](registry, C.TypeBGPBypass, NewService)
}

func NewService(ctx context.Context, logger log.ContextLogger, tag string, options option.BGPBypassServiceOptions) (adapter.Service, error) {
	if err := normalize(&options); err != nil {
		return nil, err
	}
	router := service.FromContext[adapter.Router](ctx)
	if router == nil {
		return nil, E.New("missing router")
	}
	c, err := compile(ctx, router, options)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Service{
		Adapter:   boxService.NewAdapter(C.TypeBGPBypass, tag),
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger,
		router:    router,
		dns:       service.FromContext[adapter.DNSRouter](ctx),
		outbounds: service.FromContext[adapter.OutboundManager](ctx),
		config:    c,
		requests:  make(chan learnRequest, options.Limits.MaxPendingUpdates),
		updates:   make(chan struct{}, 1),
	}
	s.state = newRouteState(c)
	s.configValid.Store(true)
	s.snapshot.Store(&controlSnapshot{
		Desired: map[netip.Prefix]bool{},
		Actual:  map[netip.Prefix]bool{},
		Peers:   map[string]bool{},
	})
	return s, nil
}

func (s *Service) ValidateConfiguration(o option.Options) error {
	if err := s.compileRouting(o); err != nil {
		return err
	}
	for _, tag := range s.config.options.DNSInbounds {
		found := false
		for _, in := range o.Inbounds {
			if in.Tag == tag {
				found = true
			}
		}
		if !found {
			return E.New("unknown dns_inbound ", strconv.Quote(tag))
		}
	}
	observable, ok := s.dns.(adapter.DNSResponseObservable)
	if !ok || !observable.DNSCacheExpires() {
		return E.New("DNS router must expose final responses and expire its cache")
	}
	if o.DNS != nil {
		for _, server := range o.DNS.Servers {
			if server.Type == C.DNSTypeFakeIP {
				return E.New("fakeip is incompatible with bgp-bypass")
			}
		}
	}
	for _, out := range s.config.outbounds {
		v, ok := s.outbounds.Outbound(out)
		if !ok {
			return E.New("unknown learned outbound ", strconv.Quote(out))
		}
		if v.Type() == C.TypeDirect || v.Type() == C.TypeBlock || v.Type() == C.TypeDNS {
			return E.New("learned outbound requires a proxy or group, got ", strconv.Quote(out))
		}
	}
	for tag, set := range s.config.sets {
		if err := s.ValidateRuleSetUpdate(tag, set.Metadata()); err != nil {
			return err
		}
	}
	// Unloaded remote sets do not suppress validation of explicit/local prefixes.
	// All sets are validated again after loading, before any advertisement.
	if _, e := s.config.buildFixed(true); e != nil {
		return e
	}
	return nil
}

func (s *Service) ValidateRuleSetUpdate(tag string, m adapter.RuleSetMetadata) error {
	if _, ok := s.config.sets[tag]; !ok {
		return nil
	}
	if m == (adapter.RuleSetMetadata{}) {
		return nil
	}
	if m.ContainsNonDNSBypassRule {
		return E.New("rule_set ", tag, " contains conditions unavailable during DNS learning")
	}
	if s.config.fixedSets[tag] && !m.IPCIDRUnion {
		return E.New("rule_set ", tag, " must be a pure destination prefix union")
	}
	return nil
}

func (s *Service) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	switch stage {
	case adapter.StartStateInitialize:
		scope.Add(s.Close)
		if len(s.config.skipped) > 0 {
			s.logger.Info("DNS preflight skips native route rules with connection-only conditions or actions: ", fmt.Sprint(s.config.skipped))
		}
		for _, set := range s.config.sets {
			set.IncRef()
			scope.Add(func() error {
				set.DecRef()
				return nil
			})
			handle := set.RegisterCallback(func(adapter.RuleSet) {
				select {
				case s.updates <- struct{}{}:
				default:
				}
			})
			scope.Add(func() error {
				set.UnregisterCallback(handle)
				return nil
			})
		}
		s.router.AppendTracker(s)
	case adapter.StartStateStarted:
		if err := s.refreshRules(); err != nil {
			return err
		}
		s.loadState()
		if s.config.options.Advertise {
			if s.backend == nil {
				s.backend = newBGPBackend(s.config.options.BGP, s.logger)
			}
			if err := s.backend.Start(s.ctx); err != nil {
				return err
			}
		}
		// Scope cancellation must also stop workers if another component fails.
		stop := context.AfterFunc(scope.Context(), s.cancel)
		scope.Add(func() error {
			stop()
			return nil
		})
		s.running.Store(true)
		s.wg.Go(s.run)
		unregister := s.dns.(adapter.DNSResponseObservable).RegisterDNSResponseObserver(s)
		scope.Add(func() error {
			unregister()
			return nil
		})
		s.startDNSProbe()
		s.wg.Go(func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
					s.saveState()
				}
			}
		})
	}
	return nil
}

func (s *Service) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	s.running.Store(false)
	s.cancel()
	s.wg.Wait()
	var err error
	if s.backend != nil {
		err = s.backend.Close()
	}
	s.saveState()
	return err
}

func (s *Service) refreshRules() error {
	for tag, set := range s.config.sets {
		if err := s.ValidateRuleSetUpdate(tag, set.Metadata()); err != nil {
			return err
		}
	}
	fixed, err := s.config.buildFixed()
	if err != nil {
		return err
	}
	s.state.fixedAccess.Lock()
	s.state.fixed = fixed
	s.state.fixedAccess.Unlock()
	s.state.recompute()
	s.configValid.Store(true)
	return nil
}

func (s *Service) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	s.reconcile(time.Now())
	for {
		select {
		case <-s.ctx.Done():
			return
		case req := <-s.requests:
			requests := []learnRequest{req}
			for len(requests) < 128 {
				select {
				case next := <-s.requests:
					requests = append(requests, next)
				default:
					goto drained
				}
			}
		drained:
			s.processRequests(requests)
		case <-tick.C:
			s.state.expire(time.Now())
			s.reconcile(time.Now())

		case <-s.updates:
			if err := s.refreshRules(); err != nil {
				s.logger.Error("rule-set update rejected: ", err)
				s.suspendRoutes(err)
			} else {
				s.reconcile(time.Now())
			}
		}
	}
}

func (s *Service) processRequests(requests []learnRequest) {
	now := time.Now()
	routeSlots := s.state.routeSlots.Load()
	dirty := false
	for _, request := range requests {
		if request.response != nil {
			changed, rejected := request.response.learn(now)
			dirty = dirty || changed
			s.stats.capacityRejected.Add(rejected)
			continue
		}
		for _, a := range request.values {
			if !s.state.known(a, now) {
				dirty = true
			}
			if s.config.excludedIP(a.Address) {
				continue
			}
			if !s.state.learn(a, now) {
				s.stats.capacityRejected.Add(1)
			}
		}
	}
	changed := false
	// A known lease can regain admission after a capacity-limited update.
	if dirty || s.state.routeSlots.Load() != routeSlots || s.backend != nil && !s.snapshot.Load().Converged {
		changed = s.reconcile(now)
	}
	for _, request := range requests {
		request.done <- changed
	}
}

func (s *Service) suspendRoutes(cause error) {
	if s.backend != nil {
		actual, e := s.backend.List()
		if e == nil {
			prefixes := slices.Collect(maps.Keys(actual))
			if len(prefixes) > 0 {
				_ = s.backend.Delete(prefixes)
			}
		}
	}
	s.configValid.Store(false)
	s.snapshot.Store(&controlSnapshot{
		Desired:   map[netip.Prefix]bool{},
		Actual:    map[netip.Prefix]bool{},
		Peers:     map[string]bool{},
		LastError: cause.Error(),
		At:        time.Now(),
	})
}

func (s *Service) reconcile(now time.Time) bool {
	desired := s.state.desired(now)
	if !s.configValid.Load() {
		desired = map[netip.Prefix]bool{}
	}
	snapshot := &controlSnapshot{
		Desired:     desired,
		Actual:      map[netip.Prefix]bool{},
		SettleUntil: map[netip.Prefix]time.Time{},
		Peers:       map[string]bool{},
		At:          now,
	}
	// Publish deadlines with the RIB so every DNS response observes the same
	// window. Copy only pending deadlines; published snapshots are immutable.
	settleNow := time.Now()
	if previous := s.snapshot.Load(); previous != nil {
		for p, until := range previous.SettleUntil {
			if until.After(settleNow) {
				snapshot.SettleUntil[p] = until
			}
		}
	}
	if s.backend == nil {
		s.snapshot.Store(snapshot)
		return false
	}
	actual, err := s.backend.List()
	if err != nil {
		snapshot.LastError = err.Error()
		s.stats.bgpErrors.Add(1)
		s.snapshot.Store(snapshot)
		return false
	}
	for p := range snapshot.SettleUntil {
		if !actual[p] {
			delete(snapshot.SettleUntil, p)
		}
	}
	var add, remove []netip.Prefix
	for p := range actual {
		if !desired[p] {
			remove = append(remove, p)
		}
	}
	for p := range desired {
		if !actual[p] {
			add = append(add, p)
		}
	}
	// Withdrawal takes priority. Limit each native API operation to a small batch.
	for batch := range slices.Chunk(remove, 256) {
		if e := s.backend.Delete(batch); e != nil {
			err = e
			break
		}
		s.stats.withdrawn.Add(uint64(len(batch)))
	}
	if err == nil {
		for batch := range slices.Chunk(add, 256) {
			if e := s.backend.Add(batch); e != nil {
				err = e
				break
			}
			s.stats.announced.Add(uint64(len(batch)))
			// Keep successful submissions even if the RIB readback fails.
			if delay := time.Duration(*s.config.options.Timing.SettleDelay); delay > 0 {
				until := time.Now().Add(delay)
				for _, p := range batch {
					snapshot.SettleUntil[p] = until
				}
			}
		}
	}
	if err != nil {
		s.stats.bgpErrors.Add(1)
		s.logger.Warn("BGP update failed; reconciling actual RIB: ", err)
		snapshot.LastError = err.Error()
	}
	current, listErr := s.backend.List()
	if listErr != nil {
		snapshot.LastError = listErr.Error()
	} else {
		snapshot.Actual = current
		for p := range snapshot.SettleUntil {
			if !current[p] {
				delete(snapshot.SettleUntil, p)
			}
		}
		// An Add error may still have committed part of its batch.
		if delay := time.Duration(*s.config.options.Timing.SettleDelay); delay > 0 {
			until := time.Now().Add(delay)
			for p := range current {
				if !actual[p] && desired[p] && snapshot.SettleUntil[p].IsZero() {
					snapshot.SettleUntil[p] = until
				}
			}
		}
	}
	snapshot.Peers = s.backend.Peers()
	snapshot.Converged = err == nil && listErr == nil && maps.Equal(desired, current)
	s.snapshot.Store(snapshot)
	s.stats.reconciliations.Add(1)
	for p := range current {
		if !actual[p] && desired[p] {
			return true
		}
	}
	return false
}
