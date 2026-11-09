package overrides

import (
	"flag"
	"fmt"
	"time"

	"github.com/grafana/tempo/pkg/util"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/backend/azure"
	"github.com/grafana/tempo/tempodb/backend/gcs"
	"github.com/grafana/tempo/tempodb/backend/local"
	"github.com/grafana/tempo/tempodb/backend/s3"
	"github.com/xscaler/dskit/xstenant"
)

const adminStoreFlagPrefix = "xscaler.admin-store"

// AdminStoreConfig configures the xscaler admin store as a source of per-tenant overrides.
// The admin store lives in its own bucket, separate from the trace storage.
type AdminStoreConfig struct {
	Enabled      bool          `yaml:"enabled" json:"enabled"`
	CacheDir     string        `yaml:"cache_dir" json:"cache_dir"`
	PollInterval time.Duration `yaml:"poll_interval" json:"poll_interval"`
	StartupWait  time.Duration `yaml:"startup_wait" json:"startup_wait"`
	// EnforceState selects the push gate: off, shadow or on. With on, the distributor rejects
	// pushes from tenants whose published state is soft_locked. Shadow only counts them.
	EnforceState string `yaml:"enforce_state" json:"enforce_state"`

	Backend string        `yaml:"backend" json:"backend"`
	Local   *local.Config `yaml:"local" json:"local"`
	GCS     *gcs.Config   `yaml:"gcs" json:"gcs"`
	S3      *s3.Config    `yaml:"s3" json:"s3"`
	Azure   *azure.Config `yaml:"azure" json:"azure"`
}

func (c *AdminStoreConfig) RegisterFlagsAndApplyDefaults(f *flag.FlagSet) {
	prefix := adminStoreFlagPrefix
	f.BoolVar(&c.Enabled, util.PrefixConfig(prefix, "enabled"), false, "Read per-tenant overrides from the xscaler admin store.")
	f.StringVar(&c.CacheDir, util.PrefixConfig(prefix, "cache-dir"), "", "Directory that keeps the last applied admin store generation across restarts.")
	f.DurationVar(&c.PollInterval, util.PrefixConfig(prefix, "poll-interval"), 5*time.Second, "How often to poll the admin store manifest.")
	f.DurationVar(&c.StartupWait, util.PrefixConfig(prefix, "startup-wait"), 30*time.Second, "How long to wait for the first admin store snapshot at startup before continuing with the default overrides. Must be positive. Components that delete blocks (backend-worker, backend-scheduler) wait without a timeout. In single-binary mode, where these run in the same process as the other modules, /ready stays not ready until the first snapshot is applied while the other modules keep serving.")
	f.StringVar(&c.EnforceState, util.PrefixConfig(prefix, "enforce-state"), xstenant.EnforceOff.String(), "Push gate for tenants soft locked by billing: off, shadow (count only) or on (reject pushes with RESOURCE_EXHAUSTED).")
	f.StringVar(&c.Backend, util.PrefixConfig(prefix, "backend"), "", "Backend of the admin store bucket: local, gcs, s3 or azure.")

	c.Azure = &azure.Config{}
	c.Azure.RegisterFlagsAndApplyDefaults(prefix, f)
	c.S3 = &s3.Config{}
	c.S3.RegisterFlagsAndApplyDefaults(prefix, f)
	c.GCS = &gcs.Config{}
	c.GCS.RegisterFlagsAndApplyDefaults(prefix, f)
	c.Local = &local.Config{}
	c.Local.RegisterFlagsAndApplyDefaults(prefix, f)
}

// cacheKey identifies the admin store bucket and prefix, so a disk cache written for another
// bucket is ignored.
func (c *AdminStoreConfig) cacheKey() string {
	var bucket, prefix string
	switch c.Backend {
	case backend.Local:
		if c.Local != nil {
			bucket = c.Local.Path
		}
	case backend.GCS:
		if c.GCS != nil {
			bucket, prefix = c.GCS.BucketName, c.GCS.Prefix
		}
	case backend.S3:
		if c.S3 != nil {
			bucket, prefix = c.S3.Bucket, c.S3.Prefix
		}
	case backend.Azure:
		if c.Azure != nil {
			bucket, prefix = c.Azure.StorageAccountName+"/"+c.Azure.ContainerName, c.Azure.Prefix
		}
	}
	return c.Backend + "://" + bucket + "/" + prefix + "/" + xstenant.DefaultConfig().Prefix
}

func (c *AdminStoreConfig) validate() error {
	if c.PollInterval <= 0 {
		return fmt.Errorf("xscaler_admin_store: poll_interval must be positive")
	}
	if c.StartupWait <= 0 {
		return fmt.Errorf("xscaler_admin_store: startup_wait must be positive")
	}

	if _, err := xstenant.ParseEnforceMode(c.EnforceState); c.EnforceState != "" && err != nil {
		return fmt.Errorf("xscaler_admin_store: enforce_state: %w", err)
	}

	switch c.Backend {
	case backend.Local, backend.GCS, backend.S3, backend.Azure:
		return nil
	default:
		return fmt.Errorf("xscaler_admin_store: unknown backend %q", c.Backend)
	}
}
