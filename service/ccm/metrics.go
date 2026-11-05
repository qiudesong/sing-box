package ccm

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	E "github.com/sagernet/sing/common/exceptions"
)

type usageMetricSnapshot struct {
	Stats UsageStats
	Cost  float64
}

type usageMetrics struct {
	mutex       sync.Mutex
	users       []string
	totals      map[string]usageMetricSnapshot
	current     map[string]usageMetricSnapshot
	period      map[string]usageMetricSnapshot
	periodStart int64
}

// Initialized before the service starts accepting requests.
func (s *Service) InitializeMetrics(users []string) {
	s.usageMetrics = &usageMetrics{
		users:   slices.Clone(users),
		totals:  map[string]usageMetricSnapshot{"": {}},
		current: make(map[string]usageMetricSnapshot),
		period:  make(map[string]usageMetricSnapshot),
	}
}

func (s *Service) usageTrackingEnabled() bool {
	return s.usageTracker != nil || s.usageMetrics != nil
}

func (s *Service) restoreUsageMetrics() {
	if s.usageTracker != nil && s.usageMetrics != nil {
		// History is loaded before requests start; the aggregator retains only sums.
		s.usageMetrics.restore(s.usageTracker.Combinations)
	}
}

func (s *Service) recordUsage(
	model string,
	contextWindow int,
	messagesCount int,
	inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, cacheCreation5MinuteTokens, cacheCreation1HourTokens int64,
	user string,
	observedAt time.Time,
	cycleHint *WeeklyCycleHint,
) error {
	if model == "" {
		return E.New("model cannot be empty")
	}
	if contextWindow <= 0 {
		return E.New("contextWindow must be positive")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if s.usageTracker != nil {
		err := s.usageTracker.AddUsageWithCycleHint(model, contextWindow, messagesCount, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens, cacheCreation5MinuteTokens, cacheCreation1HourTokens, user, observedAt, cycleHint)
		if err != nil {
			return err
		}
	}
	if s.usageMetrics != nil {
		if cacheCreationTokens == 0 {
			cacheCreationTokens = cacheCreation5MinuteTokens + cacheCreation1HourTokens
		}
		stats := UsageStats{
			RequestCount:                    1,
			MessagesCount:                   messagesCount,
			InputTokens:                     inputTokens,
			OutputTokens:                    outputTokens,
			CacheReadInputTokens:            cacheReadTokens,
			CacheCreationInputTokens:        cacheCreationTokens,
			CacheCreation5MinuteInputTokens: cacheCreation5MinuteTokens,
			CacheCreation1HourInputTokens:   cacheCreation1HourTokens,
		}
		s.usageMetrics.observe(user, stats, calculateRawCost(stats, model, contextWindow), deriveWeekStartUnix(cycleHint))
	}
	return nil
}

func addMetricUsage(target map[string]usageMetricSnapshot, user string, stats UsageStats, cost float64) {
	value := target[user]
	value.Stats.RequestCount += stats.RequestCount
	value.Stats.MessagesCount += stats.MessagesCount
	value.Stats.InputTokens += stats.InputTokens
	value.Stats.OutputTokens += stats.OutputTokens
	value.Stats.CacheReadInputTokens += stats.CacheReadInputTokens
	value.Stats.CacheCreationInputTokens += stats.CacheCreationInputTokens
	value.Stats.CacheCreation5MinuteInputTokens += stats.CacheCreation5MinuteInputTokens
	value.Stats.CacheCreation1HourInputTokens += stats.CacheCreation1HourInputTokens
	value.Cost += cost
	target[user] = value
}

func (u *usageMetrics) restore(combinations []CostCombination) {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	u.totals = map[string]usageMetricSnapshot{"": {}}
	u.period = make(map[string]usageMetricSnapshot)
	u.periodStart = 0
	for _, combination := range combinations {
		cost := calculateRawCost(combination.Total, combination.Model, combination.ContextWindow)
		addMetricUsage(u.totals, "", combination.Total, cost)
		u.addPeriodMetrics(combination.WeekStartUnix, "", combination.Total, cost)
		for _, user := range u.users {
			if user == "" {
				continue
			}
			stats, ok := combination.ByUser[user]
			if !ok {
				continue
			}
			cost := calculateRawCost(stats, combination.Model, combination.ContextWindow)
			addMetricUsage(u.totals, user, stats, cost)
			u.addPeriodMetrics(combination.WeekStartUnix, user, stats, cost)
		}
	}
}

func (u *usageMetrics) observe(user string, stats UsageStats, cost float64, period int64) {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	addMetricUsage(u.totals, "", stats, cost)
	u.addPeriodMetrics(period, "", stats, cost)
	addMetricUsage(u.current, "", stats, cost)
	if user == "" || !slices.Contains(u.users, user) {
		return
	}
	addMetricUsage(u.totals, user, stats, cost)
	u.addPeriodMetrics(period, user, stats, cost)
	addMetricUsage(u.current, user, stats, cost)
}

func (u *usageMetrics) addPeriodMetrics(period int64, user string, stats UsageStats, cost float64) {
	if period <= 0 || period < u.periodStart {
		return
	}
	if period > u.periodStart {
		u.periodStart = period
		u.period = make(map[string]usageMetricSnapshot)
	}
	addMetricUsage(u.period, user, stats, cost)
}

func (s *Service) CollectMetrics(emit func(adapter.Metric), _ []string) {
	if s.usageMetrics == nil {
		return
	}
	s.usageMetrics.collectMetrics(s.Tag(), emit)
}

func (u *usageMetrics) collectMetrics(serviceTag string, emit func(adapter.Metric)) {
	u.mutex.Lock()
	totals := maps.Clone(u.totals)
	current := maps.Clone(u.current)
	periodStart := u.periodStart
	period := maps.Clone(u.period)
	u.mutex.Unlock()
	if periodStart > 0 {
		emit(adapter.Metric{
			Name:   C.MetricNameCCMPeriodStartTimestamp,
			Type:   adapter.MetricGauge,
			Labels: map[string]string{C.MetricLabelService: serviceTag},
			Value:  float64(periodStart),
		})
	}
	for user, total := range totals {
		run := current[user]
		labels := map[string]string{C.MetricLabelService: serviceTag, C.MetricLabelUser: user}
		gauge := func(name string, value float64) {
			emit(adapter.Metric{
				Name:   C.MetricNamePrefixCCM + name,
				Type:   adapter.MetricGauge,
				Labels: labels,
				Value:  value,
			})
		}
		counter := func(name string, value float64) {
			emit(adapter.Metric{
				Name:   C.MetricNamePrefixCCM + name + "_total",
				Type:   adapter.MetricCounter,
				Labels: labels,
				Value:  value,
			})
		}
		gauge("requests", float64(total.Stats.RequestCount))
		if periodStart > 0 {
			gauge("period_requests", float64(period[user].Stats.RequestCount))
		}
		counter("requests", float64(run.Stats.RequestCount))
		gauge("messages", float64(total.Stats.MessagesCount))
		if periodStart > 0 {
			gauge("period_messages", float64(period[user].Stats.MessagesCount))
		}
		counter("messages", float64(run.Stats.MessagesCount))
		gauge("input_tokens", float64(total.Stats.InputTokens))
		if periodStart > 0 {
			gauge("period_input_tokens", float64(period[user].Stats.InputTokens))
		}
		counter("input_tokens", float64(run.Stats.InputTokens))
		gauge("output_tokens", float64(total.Stats.OutputTokens))
		if periodStart > 0 {
			gauge("period_output_tokens", float64(period[user].Stats.OutputTokens))
		}
		counter("output_tokens", float64(run.Stats.OutputTokens))
		gauge("cache_read_input_tokens", float64(total.Stats.CacheReadInputTokens))
		if periodStart > 0 {
			gauge("period_cache_read_input_tokens", float64(period[user].Stats.CacheReadInputTokens))
		}
		counter("cache_read_input_tokens", float64(run.Stats.CacheReadInputTokens))
		gauge("cache_creation_input_tokens", float64(total.Stats.CacheCreationInputTokens))
		if periodStart > 0 {
			gauge("period_cache_creation_input_tokens", float64(period[user].Stats.CacheCreationInputTokens))
		}
		counter("cache_creation_input_tokens", float64(run.Stats.CacheCreationInputTokens))
		gauge("cache_creation_5m_input_tokens", float64(total.Stats.CacheCreation5MinuteInputTokens))
		if periodStart > 0 {
			gauge("period_cache_creation_5m_input_tokens", float64(period[user].Stats.CacheCreation5MinuteInputTokens))
		}
		counter("cache_creation_5m_input_tokens", float64(run.Stats.CacheCreation5MinuteInputTokens))
		gauge("cache_creation_1h_input_tokens", float64(total.Stats.CacheCreation1HourInputTokens))
		if periodStart > 0 {
			gauge("period_cache_creation_1h_input_tokens", float64(period[user].Stats.CacheCreation1HourInputTokens))
		}
		counter("cache_creation_1h_input_tokens", float64(run.Stats.CacheCreation1HourInputTokens))
		gauge("cost_usd", total.Cost)
		if periodStart > 0 {
			gauge("period_cost_usd", period[user].Cost)
		}
	}
}
