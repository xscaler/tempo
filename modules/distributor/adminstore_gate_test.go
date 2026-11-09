package distributor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kitlog "github.com/go-kit/log"
	"github.com/gogo/status"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/grafana/tempo/modules/overrides"
	v1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/pkg/util/test"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/backend/local"
)

// publishTenantStates writes an admin store generation whose tenants.json holds the given states.
func publishTenantStates(t *testing.T, dir string, states map[string]string) {
	t.Helper()
	tenants := map[string]any{}
	for id, st := range states {
		tenants[id] = map[string]string{"org_id": "o-" + id, "state": st}
	}
	tenantsJSON, err := json.Marshal(map[string]any{"schema": 1, "tenants": tenants})
	require.NoError(t, err)

	objs := map[string]string{
		"tenants.json":      string(tenantsJSON),
		"keys.json":         `{"schema":1,"keys":{}}`,
		"limits/tempo.json": `{"schema":1,"tenants":{}}`,
	}
	hashes := map[string]string{}
	for name, data := range objs {
		p := filepath.Join(dir, "admin", "v1", filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
		sum := sha256.Sum256([]byte(data))
		hashes["admin/v1/"+name] = hex.EncodeToString(sum[:])
	}
	raw, err := json.Marshal(map[string]any{
		"schema": 1, "generation": 1, "published_at": time.Unix(1_800_000_001, 0).UTC(), "objects": hashes,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "admin", "v1", "manifest.json"), raw, 0o600))
}

func TestPushTracesSoftLockGate(t *testing.T) {
	// The tenant of the shared test ctx is "test".
	for _, tc := range []struct {
		name     string
		mode     string // empty leaves the admin store disabled
		state    map[string]string
		wantDeny bool
	}{
		{"flag off, admin store disabled", "", nil, false},
		{"mode off", "off", map[string]string{"test": "soft_locked"}, false},
		{"mode shadow never denies", "shadow", map[string]string{"test": "soft_locked"}, false},
		{"mode on denies soft_locked", "on", map[string]string{"test": "soft_locked"}, true},
		{"mode on allows suspended", "on", map[string]string{"test": "suspended"}, false},
		{"mode on allows active", "on", map[string]string{"test": "active"}, false},
		{"mode on allows unknown tenant", "on", map[string]string{"other": "soft_locked"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := overrides.Config{}
			cfg.RegisterFlagsAndApplyDefaults(&flag.FlagSet{})
			if tc.mode != "" {
				dir := t.TempDir()
				publishTenantStates(t, dir, tc.state)
				cfg.XscalerAdminStore = overrides.AdminStoreConfig{
					Enabled:      true,
					PollInterval: 20 * time.Millisecond,
					StartupWait:  5 * time.Second,
					Backend:      backend.Local,
					Local:        &local.Config{Path: dir},
					EnforceState: tc.mode,
				}
			}
			distributorConfig, _, l, mw := setupDependencies(t, overrides.Config{})
			overridesSvc, err := overrides.NewOverrides(cfg, nil, prometheus.NewRegistry())
			require.NoError(t, err)
			require.NoError(t, services.StartAndAwaitRunning(context.Background(), overridesSvc))
			t.Cleanup(func() {
				require.NoError(t, services.StopAndAwaitTerminated(context.Background(), overridesSvc))
			})
			d, err := New(distributorConfig, LocalPushTargets{}, nil, overridesSvc, mw, kitlog.NewNopLogger(), l, prometheus.NewPedanticRegistry())
			require.NoError(t, err)

			traces := batchesToTraces(t, []*v1.ResourceSpans{test.MakeBatch(5, []byte{})})
			_, err = d.PushTraces(ctx, traces)
			if !tc.wantDeny {
				require.NoError(t, err)
				return
			}
			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, codes.ResourceExhausted, st.Code())
			require.True(t, strings.Contains(st.Message(), "billing_soft_locked"), st.Message())
		})
	}
}
