package ccm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"

	"github.com/stretchr/testify/require"
)

func TestUsageMetricAggregationAndAllowlist(t *testing.T) {
	s := &Service{Adapter: boxService.NewAdapter(C.TypeCCM, "usage")}
	s.InitializeMetrics([]string{"allowed"})
	require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 3, 4, "allowed", time.Time{}, nil))
	require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 20, 7, 0, 0, 0, 0, "private", time.Time{}, nil))
	values := make(map[string]float64)
	s.CollectMetrics(func(metric adapter.Metric) {
		require.NotEqual(t, "private", metric.Labels["user"])
		values[metric.Name+":"+metric.Labels["user"]] = metric.Value
	}, []string{"allowed"})
	require.EqualValues(t, 30, values["sing_box_ccm_input_tokens:"])
	require.EqualValues(t, 30, values["sing_box_ccm_input_tokens_total:"])
	require.EqualValues(t, 10, values["sing_box_ccm_input_tokens:allowed"])
	require.EqualValues(t, 2, values["sing_box_ccm_requests_total:"])
	require.EqualValues(t, 7, values["sing_box_ccm_cache_creation_input_tokens:"])
	require.EqualValues(t, 7, values["sing_box_ccm_cache_creation_input_tokens_total:"])
	// Rebuilding persisted gauges must not duplicate current-lifetime counters.
	s.usageMetrics.restore(nil)
	s.CollectMetrics(func(metric adapter.Metric) {
		if metric.Name == "sing_box_ccm_requests_total" && metric.Labels["user"] == "" {
			require.EqualValues(t, 2, metric.Value)
		}
	}, nil)
}

func TestUsageMetricBillingPeriod(t *testing.T) {
	s := &Service{Adapter: boxService.NewAdapter(C.TypeCCM, "usage")}
	s.InitializeMetrics([]string{"allowed"})
	observed := time.Unix(1700000000, 0)
	hint := &WeeklyCycleHint{WindowMinutes: 7 * 24 * 60, ResetAt: observed.Add(time.Hour)}
	require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 0, 0, "allowed", observed, hint))
	hint.ResetAt = hint.ResetAt.Add(7 * 24 * time.Hour)
	require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 0, 0, "allowed", observed, hint))
	values := make(map[string]float64)
	s.CollectMetrics(func(metric adapter.Metric) {
		if metric.Labels["user"] == "" {
			values[metric.Name] = metric.Value
		}
	}, nil)
	require.EqualValues(t, 20, values["sing_box_ccm_input_tokens"])
	require.EqualValues(t, 10, values["sing_box_ccm_period_input_tokens"])
	require.EqualValues(t, 20, values["sing_box_ccm_input_tokens_total"])
	require.EqualValues(t, hint.ResetAt.Add(-7*24*time.Hour).Unix(), values["sing_box_ccm_period_start_timestamp_seconds"])
}

func TestUsageMetricCostPrecisionAndPersistence(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		requests      int
		contextWindow int
		stats         UsageStats
		extraStats    []UsageStats
		userCost      float64
		jsonTotal     float64
		jsonUser      float64
	}{
		{
			name:          "fractional cent",
			requests:      1,
			contextWindow: 200000,
			stats:         UsageStats{InputTokens: 100},
			userCost:      0.0003,
		},
		{
			name:          "many small requests",
			requests:      1000,
			contextWindow: 200000,
			stats:         UsageStats{InputTokens: 100},
			userCost:      0.3,
			jsonTotal:     0.6,
			jsonUser:      0.3,
		},
		{
			name:          "premium context",
			requests:      1,
			contextWindow: 1000000,
			stats:         UsageStats{InputTokens: 100},
			userCost:      0.0006,
		},
		{
			name:          "cache TTL split",
			requests:      1,
			contextWindow: 200000,
			stats: UsageStats{
				InputTokens:                     100,
				OutputTokens:                    10,
				CacheReadInputTokens:            100,
				CacheCreation5MinuteInputTokens: 10,
				CacheCreation1HourInputTokens:   10,
			},
			userCost: 0.0005775,
		},
		{
			name:          "legacy cache write",
			requests:      1,
			contextWindow: 200000,
			stats: UsageStats{
				InputTokens:              100,
				OutputTokens:             10,
				CacheReadInputTokens:     100,
				CacheCreationInputTokens: 20,
			},
			userCost: 0.000555,
		},
		{
			name:          "mixed legacy and 5m",
			requests:      1,
			contextWindow: 200000,
			stats:         UsageStats{InputTokens: 10, CacheCreationInputTokens: 1000},
			extraStats: []UsageStats{
				{InputTokens: 10, CacheCreationInputTokens: 1000, CacheCreation5MinuteInputTokens: 1000},
			},
			userCost:  0.00756,
			jsonTotal: 0.02,
			jsonUser:  0.01,
		},
		{
			name:          "mixed legacy and 1h",
			requests:      1,
			contextWindow: 200000,
			stats:         UsageStats{InputTokens: 10, CacheCreationInputTokens: 1000},
			extraStats: []UsageStats{
				{InputTokens: 10, CacheCreationInputTokens: 1000, CacheCreation1HourInputTokens: 1000},
			},
			userCost:  0.00981,
			jsonTotal: 0.02,
			jsonUser:  0.01,
		},
		{
			name:          "mixed legacy and TTL split",
			requests:      1,
			contextWindow: 200000,
			stats:         UsageStats{InputTokens: 10, CacheCreationInputTokens: 1000},
			extraStats: []UsageStats{
				{InputTokens: 10, CacheCreationInputTokens: 1000, CacheCreation5MinuteInputTokens: 400, CacheCreation1HourInputTokens: 600},
			},
			userCost:  0.00891,
			jsonTotal: 0.02,
			jsonUser:  0.01,
		},
		{
			name:          "TTL details exceed total",
			requests:      1,
			contextWindow: 200000,
			stats: UsageStats{
				InputTokens:                     100,
				CacheCreationInputTokens:        1,
				CacheCreation5MinuteInputTokens: 10,
				CacheCreation1HourInputTokens:   10,
			},
			userCost: 0.0003975,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s := &Service{}
			s.InitializeMetrics([]string{"allowed"})
			usage := &AggregatedUsage{ctx: context.Background(), filePath: filepath.Join(t.TempDir(), "usage.json")}
			observed := time.Unix(1700000000, 0)
			hint := &WeeklyCycleHint{WindowMinutes: weeklyWindowMinutes, ResetAt: observed.Add(time.Hour)}
			requests := append([]UsageStats{testCase.stats}, testCase.extraStats...)
			for _, user := range []string{"allowed", "private"} {
				for _, stats := range requests {
					for range testCase.requests {
						require.NoError(t, s.recordUsage(
							"claude-sonnet-4-5", testCase.contextWindow, 1,
							stats.InputTokens, stats.OutputTokens, stats.CacheReadInputTokens,
							stats.CacheCreationInputTokens, stats.CacheCreation5MinuteInputTokens,
							stats.CacheCreation1HourInputTokens, user, observed, hint,
						))
						addUsageToCombinations(&usage.Combinations, "claude-sonnet-4-5", testCase.contextWindow, deriveWeekStartUnix(hint), 1, stats.InputTokens, stats.OutputTokens, stats.CacheReadInputTokens, stats.CacheCreationInputTokens, stats.CacheCreation5MinuteInputTokens, stats.CacheCreation1HourInputTokens, user)
					}
				}
			}
			collect := func(source *usageMetrics) map[string]float64 {
				values := make(map[string]float64)
				source.collectMetrics("usage", func(metric adapter.Metric) {
					require.NotEqual(t, "private", metric.Labels[C.MetricLabelUser])
					values[metric.Name+":"+metric.Labels[C.MetricLabelUser]] = metric.Value
				})
				return values
			}
			require.Nil(t, s.usageTracker, "metrics-only mode must not retain history")
			before := collect(s.usageMetrics)
			for _, name := range []string{"cost_usd", "period_cost_usd"} {
				metricName := C.MetricNamePrefixCCM + name + ":"
				require.InDelta(t, 2*testCase.userCost, before[metricName], 1e-12)
				require.InDelta(t, testCase.userCost, before[metricName+"allowed"], 1e-12)
			}
			report := usage.ToJSON()
			require.Equal(t, testCase.jsonTotal, report.Costs.TotalUSD)
			require.Equal(t, testCase.jsonUser, report.Costs.ByUser["allowed"])
			// Persist an equivalent history without scheduling background saves.
			require.NoError(t, usage.Save())
			restored := &AggregatedUsage{ctx: context.Background(), filePath: usage.filePath}
			require.NoError(t, restored.Load())
			restoredService := &Service{usageTracker: restored}
			restoredService.InitializeMetrics([]string{"allowed"})
			restoredService.restoreUsageMetrics()
			after := collect(restoredService.usageMetrics)
			for _, name := range []string{"cost_usd", "period_cost_usd"} {
				for _, user := range []string{"", "allowed"} {
					key := C.MetricNamePrefixCCM + name + ":" + user
					require.InDelta(t, before[key], after[key], 1e-12, key)
				}
			}
			require.EqualValues(t, 2*testCase.requests*len(requests), before[C.MetricNamePrefixCCM+"requests_total:"])
			require.Zero(t, after[C.MetricNamePrefixCCM+"requests_total:"], "restoring history must not populate current-lifetime counters")
		})
	}
}

func TestUsageTrackingModes(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, metrics := range []bool{false, true} {
			t.Run(fmt.Sprintf("disk=%t/metrics=%t", disk, metrics), func(t *testing.T) {
				s := &Service{Adapter: boxService.NewAdapter(C.TypeCCM, "usage")}
				if disk {
					s.usageTracker = &AggregatedUsage{ctx: context.Background(), filePath: filepath.Join(t.TempDir(), "usage.json")}
					require.NoError(t, s.usageTracker.Load())
				}
				if metrics {
					s.InitializeMetrics(nil)
				}
				s.restoreUsageMetrics()
				require.Equal(t, disk || metrics, s.usageTrackingEnabled())
				require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 0, 0, "allowed", time.Time{}, nil))
				emitted := false
				s.CollectMetrics(func(metric adapter.Metric) {
					emitted = true
					if metric.Name == C.MetricNamePrefixCCM+"requests_total" {
						require.EqualValues(t, 1, metric.Value)
					}
				}, nil)
				require.Equal(t, metrics, emitted)
				if disk {
					require.Equal(t, 1, s.usageTracker.ToJSON().Combinations[0].Total.RequestCount)
					// Wait for the original asynchronous save to finish before removing the directory.
					require.Eventually(t, func() bool {
						s.usageTracker.saveMutex.Lock()
						defer s.usageTracker.saveMutex.Unlock()
						return !s.usageTracker.lastSaveTime.IsZero()
					}, 5*time.Second, time.Millisecond)
					s.usageTracker.cancelPendingSave()
				}
			})
		}
	}
}

func TestUsageMetricsAfterInvalidHistory(t *testing.T) {
	tracker := &AggregatedUsage{ctx: context.Background(), filePath: filepath.Join(t.TempDir(), "usage.json")}
	require.NoError(t, os.WriteFile(tracker.filePath, []byte("invalid json"), 0o600))
	s := &Service{usageTracker: tracker}
	s.InitializeMetrics(nil)
	require.Error(t, tracker.Load())
	// Match startup's handling of an unreadable history.
	s.usageTracker = nil
	s.restoreUsageMetrics()
	require.True(t, s.usageTrackingEnabled())
	require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 0, 0, "allowed", time.Time{}, nil))
	require.Equal(t, 1, s.usageMetrics.current[""].Stats.RequestCount)
}

func TestUsageMetricsConcurrentCollection(t *testing.T) {
	s := &Service{Adapter: boxService.NewAdapter(C.TypeCCM, "usage")}
	s.InitializeMetrics([]string{"allowed"})
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 100 {
				if err := s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 0, 0, "allowed", time.Time{}, nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
		workers.Go(func() {
			for range 100 {
				s.CollectMetrics(func(adapter.Metric) {}, nil)
			}
		})
	}
	workers.Wait()
	s.CollectMetrics(func(metric adapter.Metric) {
		if metric.Name == C.MetricNamePrefixCCM+"requests_total" {
			require.EqualValues(t, 400, metric.Value)
		}
	}, nil)
	// Emission runs after releasing the aggregation lock.
	once := sync.Once{}
	s.CollectMetrics(func(adapter.Metric) {
		once.Do(func() {
			require.NoError(t, s.recordUsage("claude-sonnet-4-5", 200000, 1, 10, 5, 2, 0, 0, 0, "allowed", time.Time{}, nil))
		})
	}, nil)
}

func TestUsageMetricsOnlyResponses(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		contentType string

		body string
	}{
		{"json", "application/json", `{"model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":5}}`},
		{"sse", "text/event-stream", "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":10}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s := &Service{logger: log.NewNOPFactory().Logger()}
			s.InitializeMetrics([]string{"allowed"})
			response := &http.Response{
				Header: http.Header{"Content-Type": {testCase.contentType}},
				Body:   io.NopCloser(strings.NewReader(testCase.body)),
			}
			writer := httptest.NewRecorder()
			s.handleResponseWithTracking(writer, response, "claude-sonnet-4-5", "", 1, "allowed")
			require.Equal(t, testCase.body, writer.Body.String())
			require.Nil(t, s.usageTracker)
			require.EqualValues(t, 10, s.usageMetrics.current[""].Stats.InputTokens)
			require.EqualValues(t, 5, s.usageMetrics.current["allowed"].Stats.OutputTokens)
		})
	}
}
