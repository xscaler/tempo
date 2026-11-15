package livestore

import (
	"context"
	"testing"
	"time"

	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/grafana/tempo/modules/overrides"
	"github.com/grafana/tempo/pkg/util/test"
)

// liveStoreWithMaxLiveTraces starts a test live-store (background processes
// held, so only consume() can cut) with a per-tenant live-trace limit.
func liveStoreWithMaxLiveTraces(t *testing.T, maxLiveTraces int) *LiveStore {
	t.Helper()
	cfg := defaultConfig(t, t.TempDir())
	limits, err := overrides.NewOverrides(overrides.Config{
		Defaults: overrides.Overrides{
			Ingestion: overrides.IngestionOverrides{MaxLocalTracesPerUser: maxLiveTraces},
		},
	}, nil, prometheus.NewRegistry())
	require.NoError(t, err)

	ls, err := New(cfg, limits, noopCompleteBlockFlusher{}, test.NewTestingLogger(t), prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(t.Context(), ls))
	t.Cleanup(func() {
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), ls))
	})
	return ls
}

// recordsAt returns n records for tenant, one single-trace push each, with
// timestamps from start spaced step apart.
func recordsAt(t *testing.T, tenant string, n int, start time.Time, step time.Duration) []*kgo.Record {
	t.Helper()
	records := make([]*kgo.Record, 0, n)
	for i := range n {
		records = append(records, &kgo.Record{
			Key:       []byte(tenant),
			Timestamp: start.Add(time.Duration(i) * step),
			Value:     createValidPushRequest(t),
		})
	}
	return records
}

// tracesKept cuts every remaining live trace and returns how many traces the
// tenant wrote to its head block in total, which is every trace it kept.
func tracesKept(t *testing.T, ls *LiveStore, tenant string) int {
	t.Helper()
	inst, err := ls.getOrCreateInstance(tenant)
	require.NoError(t, err)
	ls.cutOneInstanceToWal(t.Context(), inst, true)
	return int(test.MustGetCounterValue(metricTracesCreatedTotal.WithLabelValues(tenant)))
}

// A replay of more traces than max_traces_per_user, spread over minutes of
// record time, keeps every trace: consume() cuts idle traces by record time.
// Before the fix the replay filled the limit and discarded the rest
// (xscaler/tempo#16).
func TestConsumeCatchUpKeepsTracesOverLiveTraceLimit(t *testing.T) {
	const (
		tenant        = "catchup-over-limit"
		maxLiveTraces = 100
		numTraces     = 500
	)
	ls := liveStoreWithMaxLiveTraces(t, maxLiveTraces)

	now := time.Now()
	// 500 traces over 10 minutes of record time, starting 12 minutes ago:
	// inside complete_block_timeout (20m), so none is dropped as too old.
	records := recordsAt(t, tenant, numTraces, now.Add(-12*time.Minute), 1200*time.Millisecond)

	_, err := ls.consume(t.Context(), createRecordIter(records), now)
	require.NoError(t, err)

	require.Equal(t, numTraces, tracesKept(t, ls, tenant), "every replayed trace must be kept")
	require.Positive(t, test.MustGetCounterValue(metricCatchUpCuts.WithLabelValues(tenant)), "catch-up cuts must run")

	// Memory stays bounded during the replay: live traces never reach the limit.
	inst, err := ls.getOrCreateInstance(tenant)
	require.NoError(t, err)
	require.Less(t, inst.liveTraces.Len(), uint64(maxLiveTraces))
}

// Records at wall-clock time (no lag) take the normal path: no catch-up cuts,
// and the live-trace limit still applies to a burst.
func TestConsumeNoLagKeepsLiveTraceLimit(t *testing.T) {
	const (
		tenant        = "no-lag-burst"
		maxLiveTraces = 100
		numTraces     = 500
	)
	ls := liveStoreWithMaxLiveTraces(t, maxLiveTraces)

	now := time.Now()
	records := recordsAt(t, tenant, numTraces, now, 0)

	_, err := ls.consume(t.Context(), createRecordIter(records), now)
	require.NoError(t, err)

	require.Equal(t, maxLiveTraces, tracesKept(t, ls, tenant), "a live burst is still capped by the limit")
	require.Zero(t, test.MustGetCounterValue(metricCatchUpCuts.WithLabelValues(tenant)), "no catch-up cuts without lag")
}

// needsCatchUpCut fires once per flush period of record time, and early when
// live traces reach 3/4 of the limit.
func TestNeedsCatchUpCut(t *testing.T) {
	ls := liveStoreWithMaxLiveTraces(t, 8)
	inst, err := ls.getOrCreateInstance("needs-cut")
	require.NoError(t, err)

	base := time.Now().Add(-10 * time.Minute)
	period := 5 * time.Second

	require.True(t, inst.needsCatchUpCut(base, period), "first lagging record cuts")
	require.False(t, inst.needsCatchUpCut(base.Add(time.Second), period), "within the period, below the limit")
	require.True(t, inst.needsCatchUpCut(base.Add(period), period), "one period of record time later")

	// Fill live traces to 3/4 of the limit (6 of 8) without advancing record time.
	at := base.Add(period)
	for _, r := range recordsAt(t, "needs-cut", 6, at, 0) {
		req, err := ls.decoder.Decode(r.Value)
		require.NoError(t, err)
		inst.pushBytes(t.Context(), at, req)
		ls.decoder.Reset()
	}
	require.EqualValues(t, 6, inst.liveTraces.Len())
	require.True(t, inst.needsCatchUpCut(at.Add(time.Millisecond), period), "at 3/4 of the limit")
}
