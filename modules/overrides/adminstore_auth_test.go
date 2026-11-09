package overrides

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/xscaler/dskit/xstenant"
)

const authToken = "tok-a"

// publishAuthStore publishes a generation with one read/write key for tenant "t-a" and one
// read-only key for tenant "t-b".
func publishAuthStore(t *testing.T, dir string) {
	t.Helper()
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	keys := map[string]any{"schema": 1, "keys": map[string]any{
		hash(authToken): map[string]any{"tenant_id": "t-a", "scopes": []string{"read", "write"}},
		hash("tok-b"):   map[string]any{"tenant_id": "t-b", "scopes": []string{"read"}},
	}}
	keysRaw, err := json.Marshal(keys)
	require.NoError(t, err)
	objs := map[string]string{
		"tenants.json":      `{"schema":1,"tenants":{}}`,
		"keys.json":         string(keysRaw),
		"limits/tempo.json": `{"schema":1,"tenants":{}}`,
	}
	hashes := map[string]string{}
	for name, data := range objs {
		writeAdminStoreFile(t, dir, adminStorePrefix+name, []byte(data))
		hashes[adminStorePrefix+name] = hash(data)
	}
	raw, err := json.Marshal(map[string]any{
		"schema": 1, "generation": 1, "published_at": time.Unix(1_800_000_001, 0).UTC(), "objects": hashes,
	})
	require.NoError(t, err)
	writeAdminStoreFile(t, dir, adminStorePrefix+"manifest.json", raw)
}

func authReq(path, token, orgID string) xstenant.AuthRequest {
	return xstenant.AuthRequest{
		Kind:          xstenant.KindTraces,
		Method:        http.MethodGet,
		Path:          path,
		Authorization: "Bearer " + token,
		OrgID:         orgID,
	}
}

func TestAdminStoreCheckAuth(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		req          xstenant.AuthRequest
		wantAllowed  bool
		wantEnforced bool
		wantTenant   string
		wantStatus   int
	}{
		{"unset is off", "", authReq("/api/search", "bad", "t-a"), true, false, "", 0},
		{"off", "off", authReq("/api/search", "bad", "t-a"), true, false, "", 0},
		{"shadow valid", "shadow", authReq("/api/search", authToken, "t-a"), true, false, "t-a", 0},
		{"shadow invalid is not blocked", "shadow", authReq("/api/search", "bad", "t-a"), true, false, "", http.StatusUnauthorized},
		{"shadow mismatch keeps no tenant", "shadow", authReq("/api/search", authToken, "t-x"), true, false, "", http.StatusUnauthorized},
		{"on valid enforces tenant", "on", authReq("/api/traces/abc", authToken, "t-a"), true, true, "t-a", 0},
		{"on unknown token", "on", authReq("/api/search", "bad", "t-a"), false, false, "", http.StatusUnauthorized},
		{"on tenant mismatch", "on", authReq("/api/search", authToken, "t-x"), false, false, "", http.StatusUnauthorized},
		{"on path not allowed", "on", authReq("/api/overrides", authToken, "t-a"), false, false, "", http.StatusForbidden},
		{"on read key cannot push", "on", xstenant.AuthRequest{
			Kind: xstenant.KindTraces, Method: http.MethodPost, Path: "/v1/traces",
			Authorization: "Bearer tok-b", OrgID: "t-b",
		}, false, false, "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			publishAuthStore(t, dir)
			o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry(), func(c *Config) {
				c.XscalerAdminStore.EnforceAuth = tc.mode
			})
			d := CheckAuth(o, tc.req)
			require.Equal(t, tc.wantAllowed, d.Allowed)
			require.Equal(t, tc.wantEnforced, d.Enforced)
			require.Equal(t, tc.wantTenant, d.Tenant)
			require.Equal(t, tc.wantStatus, d.Status)
		})
	}
}

func TestAdminStoreCheckAuthDisabled(t *testing.T) {
	// Without the admin store nothing is checked, whatever enforce_auth says.
	cfg := Config{XscalerAdminStore: AdminStoreConfig{EnforceAuth: "on"}}
	o, err := newRuntimeConfigOverrides(cfg, &mockValidator{}, prometheus.NewRegistry())
	require.NoError(t, err)
	d := CheckAuth(o, authReq("/api/search", "bad", "t-a"))
	require.True(t, d.Allowed)
	require.False(t, d.Enforced)
}

func TestAdminStoreEnforceAuthConfig(t *testing.T) {
	var c Config
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c.RegisterFlagsAndApplyDefaults(fs)
	require.Equal(t, "off", c.XscalerAdminStore.EnforceAuth)
	require.NoError(t, fs.Parse([]string{"-xscaler.admin-store.enforce-auth=shadow"}))
	require.Equal(t, "shadow", c.XscalerAdminStore.EnforceAuth)

	bad := AdminStoreConfig{PollInterval: time.Second, StartupWait: time.Second, Backend: "local", EnforceAuth: "maybe"}
	require.ErrorContains(t, bad.validate(), "enforce_auth")
	empty := AdminStoreConfig{PollInterval: time.Second, StartupWait: time.Second, Backend: "local"}
	require.NoError(t, empty.validate())
}

func TestAdminStoreEnforceAuthParsedOnce(t *testing.T) {
	dir := t.TempDir()
	publishAuthStore(t, dir)
	o := newAdminStoreTestOverrides(t, dir, "", prometheus.NewRegistry(), func(c *Config) {
		c.XscalerAdminStore.EnforceAuth = "on"
	})
	rc, ok := o.(*runtimeConfigOverridesManager)
	require.True(t, ok)
	require.Equal(t, xstenant.EnforceOn, rc.adminStore.enforceAuth)
	rc.adminStore.cfg.EnforceAuth = "off"
	require.False(t, CheckAuth(o, authReq("/api/search", "bad", "t-a")).Allowed)
}
