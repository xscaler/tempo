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

// counter reads a global per-tenant counter. The counters are promauto
// globals and add up across test runs (-count), so tests compare deltas.
func counter(t *testing.T, c *prometheus.CounterVec, tenant string) float64 {
	t.Helper()
	return test.MustGetCounterValue(c.WithLabelValues(tenant))
}

// tracesKept cuts every remaining live trace and returns how many traces the
// tenant wrote to its head block since tracesBefore (an earlier reading of
// tempo_live_store_traces_created_total), which is every trace it kept.
func tracesKept(t *testing.T, ls *LiveStore, tenant string, tracesBefore float64) int {
	t.Helper()
	inst, err := ls.getOrCreateInstance(tenant)
	require.NoError(t, err)
	ls.cutOneInstanceToWal(t.Context(), inst, true)
	return int(counter(t, metricTracesCreatedTotal, tenant) - tracesBefore)
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
	tracesBefore := counter(t, metricTracesCreatedTotal, tenant)
	cutsBefore := counter(t, metricCatchUpCuts, tenant)

	now := time.Now()
	// 500 traces over 10 minutes of record time, starting 12 minutes ago:
	// inside complete_block_timeout (20m), so none is dropped as too old.
	records := recordsAt(t, tenant, numTraces, now.Add(-12*time.Minute), 1200*time.Millisecond)

	_, err := ls.consume(t.Context(), createRecordIter(records), now)
	require.NoError(t, err)

	// Memory stays bounded during the replay: the catch-up cuts leave fewer
	// live traces than the limit. Every trace kept below also means no push
	// reached the limit, since a push over it is discarded.
	inst, err := ls.getOrCreateInstance(tenant)
	require.NoError(t, err)
	require.Less(t, inst.liveTraces.Len(), uint64(maxLiveTraces))

	require.Equal(t, numTraces, tracesKept(t, ls, tenant, tracesBefore), "every replayed trace must be kept")
	require.Positive(t, counter(t, metricCatchUpCuts, tenant)-cutsBefore, "catch-up cuts must run")
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
	tracesBefore := counter(t, metricTracesCreatedTotal, tenant)
	cutsBefore := counter(t, metricCatchUpCuts, tenant)

	now := time.Now()
	records := recordsAt(t, tenant, numTraces, now, 0)

	_, err := ls.consume(t.Context(), createRecordIter(records), now)
	require.NoError(t, err)

	require.Equal(t, maxLiveTraces, tracesKept(t, ls, tenant, tracesBefore), "a live burst is still capped by the limit")
	require.Zero(t, counter(t, metricCatchUpCuts, tenant)-cutsBefore, "no catch-up cuts without lag")
}

// needsCatchUpCut fires once per flush period of record time, and early when
// live traces reach 3/4 of the limit. An early cut that freed nothing only
// repeats after live traces grow by another 1/8 of the limit.
func TestNeedsCatchUpCut(t *testing.T) {
	const tenant = "needs-cut"
	ls := liveStoreWithMaxLiveTraces(t, 16)
	inst, err := ls.getOrCreateInstance(tenant)
	require.NoError(t, err)

	base := time.Now().Add(-10 * time.Minute)
	period := 5 * time.Second

	require.True(t, inst.needsCatchUpCut(base, period), "first lagging record cuts")
	inst.catchUpCutDone(base)
	require.False(t, inst.needsCatchUpCut(base.Add(time.Second), period), "within the period, below the limit")
	require.True(t, inst.needsCatchUpCut(base.Add(period), period), "one period of record time later")
	inst.catchUpCutDone(base.Add(period))

	// push adds n live traces, all active at the same record time.
	at := base.Add(period)
	push := func(n int) {
		for _, r := range recordsAt(t, tenant, n, at, 0) {
			req, err := ls.decoder.Decode(r.Value)
			require.NoError(t, err)
			inst.pushBytes(t.Context(), at, req)
			ls.decoder.Reset()
		}
	}

	push(12) // 3/4 of 16
	require.EqualValues(t, 12, inst.liveTraces.Len())
	next := at.Add(time.Millisecond)
	require.True(t, inst.needsCatchUpCut(next, period), "at 3/4 of the limit")

	// The traces are active, so the cut frees nothing.
	ls.cutOneInstanceToWalAsOf(t.Context(), inst, next, false)
	inst.catchUpCutDone(next)
	require.EqualValues(t, 12, inst.liveTraces.Len())
	require.False(t, inst.needsCatchUpCut(next.Add(time.Millisecond), period), "no repeat cut without growth")

	push(2) // grows by 1/8 of 16
	require.True(t, inst.needsCatchUpCut(next.Add(2*time.Millisecond), period), "repeat cut after growth")

	// A wall-clock cut drains live traces below the count left by the last
	// catch-up cut. Growth is then measured from the lower count, so early
	// cuts resume at 3/4 of the limit instead of waiting for the next period.
	ls.cutOneInstanceToWal(t.Context(), inst, true)
	require.Zero(t, inst.liveTraces.Len())
	// consume() checks on every lagging record, so it sees the drained count.
	require.False(t, inst.needsCatchUpCut(next.Add(2*time.Millisecond), period), "nothing to cut after the drain")
	push(12)
	require.True(t, inst.needsCatchUpCut(next.Add(3*time.Millisecond), period), "early cut after a wall-clock drain")
}
