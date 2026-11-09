package overrides

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/xscaler/dskit/xstenant"
)

const gateTenants = `{"schema":1,"tenants":{
	"t-locked":    {"org_id":"o1","state":"soft_locked"},
	"t-suspended": {"org_id":"o2","state":"suspended"},
	"t-active":    {"org_id":"o3","state":"active"}
}}`

// publishGateStore publishes a generation with the given tenants.json.
func publishGateStore(t *testing.T, dir string, tenants string) {
	t.Helper()
	objs := map[string]string{
		"tenants.json":      tenants,
		"keys.json":         `{"schema":1,"keys":{}}`,
		"limits/tempo.json": `{"schema":1,"tenants":{}}`,
	}
	hashes := map[string]string{}
	for name, data := range objs {
		writeAdminStoreFile(t, dir, adminStorePrefix+name, []byte(data))
		sum := sha256.Sum256([]byte(data))
		hashes[adminStorePrefix+name] = hex.EncodeToString(sum[:])
	}
	raw, err := json.Marshal(map[string]any{
		"schema": 1, "generation": 1, "published_at": time.Unix(1_800_000_001, 0).UTC(), "objects": hashes,
	})
	require.NoError(t, err)
	writeAdminStoreFile(t, dir, adminStorePrefix+"manifest.json", raw)
}

func TestAdminStoreCheckPush(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		tenant     string
		wantDeny   bool
		wantReason string
	}{
		{"", "t-locked", false, ""},
		{"off", "t-locked", false, xstenant.ReasonSoftLocked},
		{"shadow", "t-locked", false, xstenant.ReasonSoftLocked},
		{"on", "t-locked", true, xstenant.ReasonSoftLocked},
		{"on", "t-suspended", false, xstenant.ReasonAllowed},
		{"on", "t-active", false, xstenant.ReasonAllowed},
		{"on", "t-unknown", false, xstenant.ReasonUnknownTenant},
		{"shadow", "t-unknown", false, xstenant.ReasonUnknownTenant},
	} {
		t.Run(tc.mode+"/"+tc.tenant, func(t *testing.T) {
			dir := t.TempDir()
			publishGateStore(t, dir, gateTenants)
			o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry(), func(c *Config) {
				c.XscalerAdminStore.EnforceState = tc.mode
			})
			deny, reason := CheckPush(o, tc.tenant)
			require.Equal(t, tc.wantDeny, deny)
			if tc.mode != "" {
				require.Equal(t, tc.wantReason, reason)
			}
		})
	}
}

func TestAdminStoreCheckPushDisabled(t *testing.T) {
	// Without the admin store the gate never denies, whatever enforce_state says.
	cfg := Config{XscalerAdminStore: AdminStoreConfig{EnforceState: "on"}}
	o, err := newRuntimeConfigOverrides(cfg, &mockValidator{}, prometheus.NewRegistry())
	require.NoError(t, err)
	deny, _ := CheckPush(o, "t-locked")
	require.False(t, deny)
}

func TestAdminStoreEnforceStateConfig(t *testing.T) {
	var c Config
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c.RegisterFlagsAndApplyDefaults(fs)
	require.Equal(t, "off", c.XscalerAdminStore.EnforceState)
	require.NoError(t, fs.Parse([]string{"-xscaler.admin-store.enforce-state=shadow"}))
	require.Equal(t, "shadow", c.XscalerAdminStore.EnforceState)

	bad := AdminStoreConfig{PollInterval: time.Second, StartupWait: time.Second, Backend: "local", EnforceState: "maybe"}
	require.ErrorContains(t, bad.validate(), "enforce_state")
}

func TestAdminStoreCheckPushLeavesReadsAlone(t *testing.T) {
	dir := t.TempDir()
	publishGateStore(t, dir, gateTenants)
	o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry(), func(c *Config) {
		c.XscalerAdminStore.EnforceState = "on"
	})
	deny, _ := CheckPush(o, "t-locked")
	require.True(t, deny)
	// Overrides used by the read path are served for a soft locked tenant as before.
	require.Equal(t, 24*time.Hour, o.BlockRetention("t-locked"))
	require.Equal(t, o.MaxBytesPerTagValuesQuery("t-active"), o.MaxBytesPerTagValuesQuery("t-locked"))
}

func TestParseEnforceModeOnce(t *testing.T) {
	for in, want := range map[string]xstenant.EnforceMode{
		"": xstenant.EnforceOff, "off": xstenant.EnforceOff, "shadow": xstenant.EnforceShadow, "On": xstenant.EnforceOn,
	} {
		require.Equal(t, want, parseEnforceMode(in), in)
	}

	dir := t.TempDir()
	publishGateStore(t, dir, gateTenants)
	o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry(), func(c *Config) {
		c.XscalerAdminStore.EnforceState = "on"
	})
	rc, ok := o.(*runtimeConfigOverridesManager)
	require.True(t, ok)
	require.Equal(t, xstenant.EnforceOn, rc.adminStore.enforce)
	// the stored mode is what the gate uses, the config string is not read again
	rc.adminStore.cfg.EnforceState = "off"
	deny, _ := CheckPush(o, "t-locked")
	require.True(t, deny)
}
