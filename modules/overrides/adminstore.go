package overrides

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/xscaler/dskit/xstenant"
)

const adminStoreEngine = "tempo"

// adminStoreWaiter is implemented by overrides that can wait for the admin store.
type adminStoreWaiter interface {
	waitAdminStoreApplied(ctx context.Context) error
}

// WaitAdminStoreApplied blocks until the admin store snapshot is applied to o, with no timeout,
// and returns ctx.Err() when ctx is done first. It returns at once when o does not read the
// admin store. Components that delete blocks call it before they start, so retention never
// runs on the default overrides. Other components do not wait.
func WaitAdminStoreApplied(ctx context.Context, o Interface) error {
	if w, ok := o.(adminStoreWaiter); ok {
		return w.waitAdminStoreApplied(ctx)
	}
	return nil
}

// adminStoreSource serves per-tenant overrides published to the xscaler admin store.
type adminStoreSource struct {
	services.Service

	cfg      AdminStoreConfig
	defaults *Overrides
	// defaultsJSON is the configured defaults without remote write headers, which marshal redacted.
	defaultsJSON []byte
	validator    Validator
	reader       *xstenant.Reader
	// enforce is the push gate mode, parsed once from cfg.EnforceState.
	enforce xstenant.EnforceMode
	// enforceAuth is the API key authorization mode, parsed once from cfg.EnforceAuth.
	enforceAuth xstenant.EnforceMode
	source      *xstenant.Source[*Overrides]
	logger      log.Logger

	stop func()
}

func newAdminStoreSource(cfg AdminStoreConfig, defaults *Overrides, validator Validator, bucket xstenant.Bucket, logger log.Logger, reg prometheus.Registerer) (*adminStoreSource, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Marshal the defaults once. Each tenant decodes the defaults and then its own block on top, so
	// fields the block leaves out keep the default and nothing is shared with the defaults.
	withoutSecrets := *defaults
	withoutSecrets.MetricsGenerator.RemoteWriteHeaders = nil
	defaultsJSON, err := json.Marshal(withoutSecrets)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal default overrides: %w", err)
	}

	rcfg := xstenant.DefaultConfig()
	rcfg.PollInterval = cfg.PollInterval
	rcfg.CacheDir = cfg.CacheDir
	rcfg.CacheKey = cfg.cacheKey()

	reader, err := xstenant.New(rcfg, bucket, logger, reg)
	if err != nil {
		return nil, fmt.Errorf("failed to create admin store reader: %w", err)
	}

	s := &adminStoreSource{
		cfg:          cfg,
		defaults:     defaults,
		defaultsJSON: defaultsJSON,
		validator:    validator,
		reader:       reader,
		enforce:      parseEnforceMode(cfg.EnforceState),
		enforceAuth:  parseEnforceMode(cfg.EnforceAuth),
		logger:       logger,
	}
	s.source = xstenant.NewSource(reader, adminStoreEngine, s.decode, logger, reg)

	s.Service = services.NewBasicService(s.starting, s.running, s.stopping)
	return s, nil
}

func (s *adminStoreSource) starting(ctx context.Context) error {
	s.stop = s.reader.Start(ctx)

	// Gate on the source, not on the reader: a snapshot restored from the disk cache makes the
	// reader ready before anything is decoded. The wait is bounded so that every module in the
	// process starts; components that delete blocks wait without a bound through WaitApplied.
	if err := s.source.WaitApplied(ctx, s.cfg.StartupWait); err != nil {
		if ctx.Err() != nil {
			s.stop()
			return ctx.Err()
		}
		level.Warn(s.logger).Log("msg", "admin store not applied, continuing with the default overrides", "wait", s.cfg.StartupWait, "err", err)
	}
	return nil
}

func (s *adminStoreSource) running(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *adminStoreSource) stopping(_ error) error {
	if s.stop != nil {
		s.stop()
	}
	return nil
}

// waitApplied blocks until a snapshot with the tempo limits is applied, or ctx is done.
func (s *adminStoreSource) waitApplied(ctx context.Context) error {
	return s.source.WaitApplied(ctx, 0)
}

// decode builds the overrides of one tenant: the configured defaults with the published block on top.
func (s *adminStoreSource) decode(_ string, data json.RawMessage) (*Overrides, error) {
	o := &Overrides{}
	if err := json.Unmarshal(s.defaultsJSON, o); err != nil {
		return nil, err
	}
	// Overrides.UnmarshalJSON replaces Extensions, so put back the default ones the block does not set.
	extensions := o.Extensions
	if err := json.Unmarshal(data, o); err != nil {
		return nil, err
	}
	for k, v := range extensions {
		if _, ok := o.Extensions[k]; !ok {
			if o.Extensions == nil {
				o.Extensions = make(map[string]any, len(extensions))
			}
			o.Extensions[k] = v
		}
	}
	if o.MetricsGenerator.RemoteWriteHeaders == nil {
		o.MetricsGenerator.RemoteWriteHeaders = maps.Clone(s.defaults.MetricsGenerator.RemoteWriteHeaders)
	}

	if s.validator != nil {
		warnings, err := s.validator.Validate(o)
		if err != nil {
			return nil, fmt.Errorf("validation failed: %w", err)
		}
		for _, w := range warnings {
			level.Warn(s.logger).Log("msg", "admin store overrides validation warning", "warning", w)
		}
	}
	return o, nil
}

// forUser returns the overrides published for the tenant, or nil.
func (s *adminStoreSource) forUser(userID string) *Overrides {
	o, _ := s.source.Get(userID)
	return o
}

// tenants returns the published overrides by tenant. The map must not be modified.
func (s *adminStoreSource) tenants() map[string]*Overrides {
	return s.source.All()
}
