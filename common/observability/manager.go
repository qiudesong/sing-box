package observability

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
)

type Manager struct {
	options       option.ObservabilityOptions
	logger        log.ContextLogger
	metrics       http.Handler
	Observer      adapter.MetricsObserver
	providers     []adapter.MetricsProvider
	subscriptions []metricSubscription
	mainMux       *http.ServeMux
	healthMux     *http.ServeMux
	checks        []func() bool
	running       atomic.Bool
	ready         atomic.Bool
}

func New(options option.ObservabilityOptions, logger log.ContextLogger) (*Manager, error) {
	options.MetricsUsers = slices.Clone(options.MetricsUsers)
	slices.Sort(options.MetricsUsers)
	options.MetricsUsers = slices.Compact(options.MetricsUsers)
	if err := normalizeHTTPOptions(&options); err != nil {
		return nil, err
	}

	s := &Manager{
		options:   options,
		logger:    logger,
		mainMux:   http.NewServeMux(),
		healthMux: http.NewServeMux(),
	}
	if options.Metrics {
		var err error
		s.metrics, s.Observer, err = newMetrics(s)
		if err != nil {
			return nil, err
		}
	}
	s.registerHTTPHandlers()
	return s, nil
}

func (s *Manager) Name() string { return "observability" }

// Add is called during Box construction, before listeners or collectors run.
func (s *Manager) Add(component any) {
	if subscription, ok := component.(adapter.MetricsSubscription); ok && s.options.Metrics {
		cache := new(cachedMetrics)
		s.providers = append(s.providers, cache)
		s.subscriptions = append(s.subscriptions, metricSubscription{source: subscription, cache: cache})
	}
	if provider, ok := component.(adapter.MetricsProvider); ok && s.options.Metrics {
		if initializer, ok := component.(adapter.MetricsInitializer); ok {
			initializer.InitializeMetrics(s.options.MetricsUsers)
		}
		s.providers = append(s.providers, provider)
	}
	s.addServiceHandlers(component)
}

func (s *Manager) isReady() bool {
	if !s.running.Load() || !s.ready.Load() {
		return false
	}
	for _, check := range s.checks {
		if !check() {
			return false
		}
	}
	return true
}

func (s *Manager) Stop() {
	s.ready.Store(false)
	s.running.Store(false)
}

func (s *Manager) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage == adapter.StartStateInitialize {
		scope.Add(func() error {
			s.ready.Store(false)
			s.running.Store(false)
			return nil
		})
	}
	if stage != adapter.StartStateStarted {
		return nil
	}
	ctx, cancel := context.WithCancel(scope.Context())
	var workers sync.WaitGroup
	scope.Add(func() error {
		cancel()
		workers.Wait()
		return nil
	})
	for _, subscription := range s.subscriptions {
		workers.Go(func() {
			if err := subscription.source.SubscribeMetrics(ctx, func(snapshot []adapter.Metric) {
				subscription.cache.snapshot.Store(&snapshot)
			}); err != nil && ctx.Err() == nil {
				s.logger.Error("subscribe observability: ", err)
			}
		})
	}
	if err := s.startHTTPServers(scope); err != nil {
		return err
	}
	s.running.Store(true)
	s.ready.Store(true)
	return nil
}
