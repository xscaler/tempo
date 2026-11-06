package overrides

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v2"

	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/backend/gcs"
	"github.com/grafana/tempo/tempodb/backend/local"
	"github.com/grafana/tempo/tempodb/backend/s3"
)

const adminStorePrefix = "admin/v1/"

// publishAdminStore writes a generation of the admin store to dir, objects first and manifest last.
func publishAdminStore(t *testing.T, dir string, generation int64, tempoLimits string) {
	t.Helper()

	objs := map[string]string{
		"tenants.json":      `{"schema":1,"tenants":{}}`,
		"keys.json":         `{"schema":1,"keys":{}}`,
		"limits/tempo.json": tempoLimits,
	}
	man := map[string]any{
		"schema":       1,
		"generation":   generation,
		"published_at": time.Unix(1_800_000_000+generation, 0).UTC(),
	}
	hashes := map[string]string{}
	for name, data := range objs {
		writeAdminStoreFile(t, dir, adminStorePrefix+name, []byte(data))
		sum := sha256.Sum256([]byte(data))
		hashes[adminStorePrefix+name] = hex.EncodeToString(sum[:])
	}
	man["objects"] = hashes

	raw, err := json.Marshal(man)
	require.NoError(t, err)
	writeAdminStoreFile(t, dir, adminStorePrefix+"manifest.json", raw)
}

func writeAdminStoreFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, data, 0o600))
}

func newAdminStoreTestOverrides(t *testing.T, dir, runtimeConfig string, reg *prometheus.Registry, mutate ...func(*Config)) Service {
	t.Helper()

	cfg := Config{
		Defaults: Overrides{
			Compaction: CompactionOverrides{BlockRetention: model.Duration(24 * time.Hour)},
		},
		XscalerAdminStore: AdminStoreConfig{
			Enabled:      true,
			PollInterval: 20 * time.Millisecond,
			StartupWait:  5 * time.Second,
			Backend:      backend.Local,
			Local:        &local.Config{Path: dir},
		},
	}
	if runtimeConfig != "" {
		f := filepath.Join(t.TempDir(), "overrides.yaml")
		require.NoError(t, os.WriteFile(f, []byte(runtimeConfig), 0o600))
		cfg.PerTenantOverrideConfig = f
		cfg.PerTenantOverridePeriod = model.Duration(time.Hour)
	}

	for _, m := range mutate {
		m(&cfg)
	}

	o, err := newRuntimeConfigOverrides(cfg, &mockValidator{}, reg)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	t.Cleanup(func() {
		require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))
	})
	return o
}

func TestAdminStoreBucket(t *testing.T) {
	dir := t.TempDir()
	writeAdminStoreFile(t, dir, "admin/v1/limits/tempo.json", []byte("hello"))
	writeAdminStoreFile(t, dir, "top.json", []byte("top"))

	b, err := newAdminStoreBucket(&AdminStoreConfig{Backend: backend.Local, Local: &local.Config{Path: dir}})
	require.NoError(t, err)

	for name, want := range map[string]string{"admin/v1/limits/tempo.json": "hello", "top.json": "top"} {
		rc, err := b.Get(context.Background(), name)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		require.Equal(t, want, string(got))
	}

	_, err = b.Get(context.Background(), "admin/v1/missing.json")
	require.Error(t, err)
	require.True(t, b.IsObjNotFoundErr(err))
	require.False(t, b.IsObjNotFoundErr(io.EOF))
}

func TestAdminStoreIsOnlySource(t *testing.T) {
	// Same shape the control plane publishes: nested format, partial blocks.
	const limits = `{
  "schema": 1,
  "tenants": {
    "t-admin": {"compaction": {"block_retention": "2160h"}, "ingestion": {"burst_size_bytes": 2000000}},
    "t-both": {"compaction": {"block_retention": "720h"}}
  }
}`
	const runtime = `
overrides:
  t-both:
    compaction:
      block_retention: 1h
  t-runtime:
    compaction:
      block_retention: 2h
  "*":
    compaction:
      block_retention: 3h
`

	for _, runtimeConfig := range []string{runtime, ""} {
		dir := t.TempDir()
		publishAdminStore(t, dir, 1, limits)
		o := newAdminStoreTestOverrides(t, dir, runtimeConfig, prometheus.NewRegistry())

		require.Equal(t, 2160*time.Hour, o.BlockRetention("t-admin"))
		require.Equal(t, 720*time.Hour, o.BlockRetention("t-both"), "admin store beats the runtime config")
		require.Equal(t, 24*time.Hour, o.BlockRetention("t-runtime"), "runtime config tenant is ignored")
		require.Equal(t, 24*time.Hour, o.BlockRetention("t-other"), "runtime config wildcard is ignored")
		require.Equal(t, 2000000, o.IngestionBurstSizeBytes("t-admin"))
		require.ElementsMatch(t, []string{"t-admin", "t-both"}, o.GetTenantIDs())

		reg := prometheus.NewPedanticRegistry()
		require.NoError(t, reg.Register(o))
		mfs, err := reg.Gather()
		require.NoError(t, err)
		tenants := map[string]struct{}{}
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "user" {
						tenants[l.GetValue()] = struct{}{}
					}
				}
			}
		}
		require.Equal(t, map[string]struct{}{"t-admin": {}, "t-both": {}}, tenants)
	}
}

func TestAdminStoreNewGenerationReplacesTenants(t *testing.T) {
	dir := t.TempDir()
	publishAdminStore(t, dir, 1, `{"schema":1,"tenants":{"t-a":{"compaction":{"block_retention":"48h"}}}}`)
	o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry())
	require.Equal(t, 48*time.Hour, o.BlockRetention("t-a"))

	publishAdminStore(t, dir, 2, `{"schema":1,"tenants":{"t-b":{"compaction":{"block_retention":"72h"}}}}`)
	require.Eventually(t, func() bool { return o.BlockRetention("t-b") == 72*time.Hour }, 5*time.Second, 20*time.Millisecond)
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-a"))
}

func TestAdminStoreDecodeFailure(t *testing.T) {
	dir := t.TempDir()
	publishAdminStore(t, dir, 1, `{"schema":1,"tenants":{"t-keep":{"compaction":{"block_retention":"48h"}}}}`)
	reg := prometheus.NewRegistry()
	o := newAdminStoreTestOverrides(t, dir, "", reg)
	require.Equal(t, 48*time.Hour, o.BlockRetention("t-keep"))

	publishAdminStore(t, dir, 2, `{"schema":1,"tenants":{
		"t-keep": {"compaction": {"block_retention": "not-a-duration"}},
		"t-new-bad": {"compaction": 5},
		"t-unknown": {"no_such_scope": {"x": 1}},
		"t-good": {"compaction": {"block_retention": "96h"}}
	}}`)
	require.Eventually(t, func() bool { return o.BlockRetention("t-good") == 96*time.Hour }, 5*time.Second, 20*time.Millisecond)

	require.Equal(t, 48*time.Hour, o.BlockRetention("t-keep"), "failed tenant keeps its previous value")
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-new-bad"), "failed tenant without previous value is skipped")
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-unknown"))
	require.ElementsMatch(t, []string{"t-keep", "t-good"}, o.GetTenantIDs())

	n, err := testutil.GatherAndCount(reg, "xscaler_admin_store_tenant_decode_failures_total")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, float64(3), counterValue(t, reg, "xscaler_admin_store_tenant_decode_failures_total"))
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func emptyBucketConfig(t *testing.T, mutate func(*AdminStoreConfig)) Config {
	t.Helper()
	cfg := Config{
		Defaults: Overrides{Compaction: CompactionOverrides{BlockRetention: model.Duration(24 * time.Hour)}},
		XscalerAdminStore: AdminStoreConfig{
			Enabled:      true,
			PollInterval: 20 * time.Millisecond,
			StartupWait:  200 * time.Millisecond,
			Backend:      backend.Local,
			Local:        &local.Config{Path: t.TempDir()}, // empty bucket, never ready
		},
	}
	if mutate != nil {
		mutate(&cfg.XscalerAdminStore)
	}
	return cfg
}

func TestAdminStoreStartupWaitContinues(t *testing.T) {
	o, err := newRuntimeConfigOverrides(emptyBucketConfig(t, nil), nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	require.Empty(t, o.GetTenantIDs())
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-a"))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))
}

// countingBucket counts the reads the reader makes, to tell whether it is still polling.
type countingBucket struct {
	adminStoreBucket
	gets atomic.Int64
}

func (b *countingBucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	b.gets.Add(1)
	return b.adminStoreBucket.Get(ctx, name)
}

func TestAdminStoreWaitAppliedBlocksOnlyTheCaller(t *testing.T) {
	cfg := emptyBucketConfig(t, nil)
	o, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)

	// The overrides service reaches running after startup_wait although the store is empty, so
	// other modules in the process (distributor, ingester, querier) are not blocked.
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	require.Equal(t, services.Running, o.State())

	// A component that deletes blocks waits without a bound, well past startup_wait.
	done := make(chan error, 1)
	go func() { done <- WaitAdminStoreApplied(context.Background(), o) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the admin store was applied: %v", err)
	case <-time.After(5 * cfg.XscalerAdminStore.StartupWait):
	}

	publishAdminStore(t, cfg.XscalerAdminStore.Local.Path, 1, `{"schema":1,"tenants":{"t-a":{"compaction":{"block_retention":"48h"}}}}`)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("did not return after the admin store was applied")
	}
	require.Equal(t, 48*time.Hour, o.BlockRetention("t-a"))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))
}

func TestAdminStoreWaitAppliedStopsWithContext(t *testing.T) {
	o, err := newRuntimeConfigOverrides(emptyBucketConfig(t, nil), nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitAdminStoreApplied(ctx, o), context.DeadlineExceeded)
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))
}

func TestAdminStoreWaitAppliedThroughUserConfigurable(t *testing.T) {
	cfg := emptyBucketConfig(t, nil)
	sub, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	wrapped := &userConfigurableOverridesManager{Interface: sub}

	require.NoError(t, services.StartAndAwaitRunning(context.Background(), sub))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, WaitAdminStoreApplied(ctx, wrapped), context.DeadlineExceeded)
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), sub))
}

func TestWaitAdminStoreAppliedWithoutAdminStore(t *testing.T) {
	o, err := newRuntimeConfigOverrides(Config{}, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, WaitAdminStoreApplied(context.Background(), o))
}

func TestAdminStoreStopsReaderWhenStartFails(t *testing.T) {
	cfg := emptyBucketConfig(t, nil).XscalerAdminStore
	cfg.StartupWait = time.Minute
	bucket, err := newAdminStoreBucket(&cfg)
	require.NoError(t, err)
	cb := &countingBucket{adminStoreBucket: bucket}

	src, err := newAdminStoreSource(cfg, &Overrides{}, nil, cb, log.NewNopLogger(), prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, src.StartAsync(context.Background()))
	require.Eventually(t, func() bool { return cb.gets.Load() > 0 }, 5*time.Second, 5*time.Millisecond)

	// dskit skips stopping when starting fails, so starting must stop the reader itself.
	src.StopAsync()
	awaitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.Error(t, src.AwaitTerminated(awaitCtx))
	require.Equal(t, services.Failed, src.State())

	n := cb.gets.Load()
	time.Sleep(10 * cfg.PollInterval)
	require.Equal(t, n, cb.gets.Load(), "reader is still polling after a failed start")
}

func TestAdminStoreStopsReaderOnStop(t *testing.T) {
	cfg := emptyBucketConfig(t, nil).XscalerAdminStore
	bucket, err := newAdminStoreBucket(&cfg)
	require.NoError(t, err)
	cb := &countingBucket{adminStoreBucket: bucket}

	src, err := newAdminStoreSource(cfg, &Overrides{}, nil, cb, log.NewNopLogger(), prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), src))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), src))

	n := cb.gets.Load()
	time.Sleep(10 * cfg.PollInterval)
	require.Equal(t, n, cb.gets.Load())
}

func TestAdminStoreCacheKey(t *testing.T) {
	key := func(mutate func(*AdminStoreConfig)) string {
		c := emptyBucketConfig(t, mutate).XscalerAdminStore
		return c.cacheKey()
	}
	s3Cfg := func(bucket, prefix string) func(*AdminStoreConfig) {
		return func(c *AdminStoreConfig) {
			c.Backend = backend.S3
			c.S3 = &s3.Config{Bucket: bucket, Prefix: prefix}
		}
	}

	require.Contains(t, key(s3Cfg("b1", "p")), "s3://b1/p/admin/v1/")
	require.NotEqual(t, key(s3Cfg("b1", "p")), key(s3Cfg("b2", "p")))
	require.NotEqual(t, key(s3Cfg("b1", "p")), key(s3Cfg("b1", "q")))
	require.NotEqual(t, key(s3Cfg("b1", "p")), key(func(c *AdminStoreConfig) {
		c.Backend = backend.GCS
		c.GCS = &gcs.Config{BucketName: "b1", Prefix: "p"}
	}))
}

func TestAdminStoreCacheIgnoredForOtherBucket(t *testing.T) {
	cacheDir := t.TempDir()
	withCache := func(c *AdminStoreConfig) { c.CacheDir = cacheDir }

	dir := t.TempDir()
	publishAdminStore(t, dir, 1, `{"schema":1,"tenants":{"t-a":{"compaction":{"block_retention":"48h"}}}}`)
	o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry(), func(c *Config) { withCache(&c.XscalerAdminStore) })
	require.Equal(t, 48*time.Hour, o.BlockRetention("t-a"))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))

	// another bucket, same cache dir: the cache must not be served
	cfg := emptyBucketConfig(t, withCache)
	o, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-a"))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))
}

func TestAdminStoreCacheRestoreSatisfiesWait(t *testing.T) {
	cacheDir := t.TempDir()
	withCache := func(c *AdminStoreConfig) { c.CacheDir = cacheDir }

	dir := t.TempDir()
	publishAdminStore(t, dir, 1, `{"schema":1,"tenants":{"t-a":{"compaction":{"block_retention":"48h"}}}}`)
	cfg := emptyBucketConfig(t, withCache)
	cfg.XscalerAdminStore.Local.Path = dir
	o, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	require.NoError(t, WaitAdminStoreApplied(context.Background(), o))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))

	// same bucket path but now empty and unreachable data: the cache is decoded at construction
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "admin")))
	o, err = newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, WaitAdminStoreApplied(ctx, o))
	require.Equal(t, 48*time.Hour, o.BlockRetention("t-a"))
	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), o))
}

func TestAdminStoreDisabledByDefault(t *testing.T) {
	var cfg Config
	cfg.RegisterFlagsAndApplyDefaults(flag.NewFlagSet("test", flag.ContinueOnError))
	require.False(t, cfg.XscalerAdminStore.Enabled)
	require.Equal(t, 5*time.Second, cfg.XscalerAdminStore.PollInterval)
	require.Equal(t, 30*time.Second, cfg.XscalerAdminStore.StartupWait)

	o, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.Nil(t, o.(*runtimeConfigOverridesManager).adminStore)
	require.Nil(t, o.GetTenantIDs())
}

func TestAdminStoreConfigValidation(t *testing.T) {
	cfg := Config{XscalerAdminStore: AdminStoreConfig{Enabled: true, PollInterval: time.Second, StartupWait: time.Second, Backend: "nope"}}
	_, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.ErrorContains(t, err, "unknown backend")

	for _, d := range []time.Duration{0, -time.Second} {
		c := AdminStoreConfig{Enabled: true, PollInterval: time.Second, StartupWait: d, Backend: backend.Local}
		require.ErrorContains(t, c.validate(), "startup_wait must be positive")
	}
}

func TestAdminStoreConfigYAML(t *testing.T) {
	var cfg Config
	cfg.RegisterFlagsAndApplyDefaults(flag.NewFlagSet("test", flag.ContinueOnError))
	require.NoError(t, yaml.UnmarshalStrict([]byte(`
xscaler_admin_store:
  enabled: true
  cache_dir: /var/cache/admin
  poll_interval: 2s
  backend: s3
  s3:
    bucket: admin
    endpoint: s3.local
`), &cfg))
	require.True(t, cfg.XscalerAdminStore.Enabled)
	require.Equal(t, 2*time.Second, cfg.XscalerAdminStore.PollInterval)
	require.Equal(t, "admin", cfg.XscalerAdminStore.S3.Bucket)
	require.Equal(t, 30*time.Second, cfg.XscalerAdminStore.StartupWait)
}

func TestAdminStorePartialBlockKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	publishAdminStore(t, dir, 1, `{"schema":1,"tenants":{
		"t-partial": {"compaction": {"block_retention": "48h"}, "metrics_generator": {"processor": {"span_metrics": {"dimensions": ["b"]}}}}
	}}`)

	retry := true
	cfg := Config{
		Defaults: Overrides{
			Ingestion:  IngestionOverrides{RateLimitBytes: 1234, BurstSizeBytes: 5678, RetryInfoEnabled: &retry},
			Compaction: CompactionOverrides{BlockRetention: model.Duration(24 * time.Hour)},
			Forwarders: []string{"fwd"},
			MetricsGenerator: MetricsGeneratorOverrides{
				Processor: ProcessorOverrides{SpanMetrics: SpanMetricsOverrides{Dimensions: []string{"a"}}},
			},
		},
		XscalerAdminStore: AdminStoreConfig{
			Enabled: true, PollInterval: 20 * time.Millisecond, StartupWait: 5 * time.Second,
			Backend: backend.Local, Local: &local.Config{Path: dir},
		},
	}
	o, err := newRuntimeConfigOverrides(cfg, nil, prometheus.NewRegistry())
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), o))
	t.Cleanup(func() { _ = services.StopAndAwaitTerminated(context.Background(), o) })

	require.Equal(t, 48*time.Hour, o.BlockRetention("t-partial"))    // set field overrides
	require.Equal(t, 1234.0, o.IngestionRateLimitBytes("t-partial")) // unset field keeps default
	require.Equal(t, 5678, o.IngestionBurstSizeBytes("t-partial"))
	require.Equal(t, []string{"fwd"}, o.Forwarders("t-partial"))
	require.Equal(t, []string{"b"}, o.MetricsGeneratorProcessorSpanMetricsDimensions("t-partial"))

	// the tenant copy shares nothing with the defaults
	got := o.GetRuntimeOverridesFor("t-partial")
	require.NotSame(t, cfg.Defaults.Ingestion.RetryInfoEnabled, got.Ingestion.RetryInfoEnabled)
	*got.Ingestion.RetryInfoEnabled = false
	got.Forwarders[0] = "changed"
	require.True(t, retry)
	require.Equal(t, []string{"fwd"}, cfg.Defaults.Forwarders)
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-other"))
}

func TestAdminStoreDecodeSharesNothingWithDefaults(t *testing.T) {
	retry := true
	defaults := &Overrides{
		Ingestion:  IngestionOverrides{RetryInfoEnabled: &retry},
		Forwarders: []string{"x"},
		MetricsGenerator: MetricsGeneratorOverrides{
			Processor: ProcessorOverrides{SpanMetrics: SpanMetricsOverrides{
				Dimensions:          []string{"d"},
				IntrinsicDimensions: map[string]bool{"k": true},
			}},
			RemoteWriteHeaders: RemoteWriteHeaders{"h": "secret"},
		},
	}
	src, err := newAdminStoreSource(AdminStoreConfig{PollInterval: time.Second, StartupWait: time.Second, Backend: backend.Local}, defaults, nil, adminStoreBucket{}, nil, nil)
	require.NoError(t, err)

	a, err := src.decode("a", json.RawMessage(`{"compaction":{"block_retention":"48h"}}`))
	require.NoError(t, err)
	b, err := src.decode("b", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, defaults.MetricsGenerator, a.MetricsGenerator, "defaults survive, remote write headers included")
	require.Equal(t, defaults.Forwarders, a.Forwarders)

	*a.Ingestion.RetryInfoEnabled = false
	a.Forwarders[0] = "y"
	a.MetricsGenerator.Processor.SpanMetrics.Dimensions[0] = "y"
	a.MetricsGenerator.Processor.SpanMetrics.IntrinsicDimensions["k"] = false
	a.MetricsGenerator.RemoteWriteHeaders["h"] = "other"

	for _, o := range []*Overrides{defaults, b} {
		require.True(t, *o.Ingestion.RetryInfoEnabled)
		require.Equal(t, []string{"x"}, o.Forwarders)
		require.Equal(t, []string{"d"}, o.MetricsGenerator.Processor.SpanMetrics.Dimensions)
		require.True(t, o.MetricsGenerator.Processor.SpanMetrics.IntrinsicDimensions["k"])
		require.EqualValues(t, "secret", o.MetricsGenerator.RemoteWriteHeaders["h"])
	}
}
