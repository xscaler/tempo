package overrides

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/backend/azure"
	"github.com/grafana/tempo/tempodb/backend/gcs"
	"github.com/grafana/tempo/tempodb/backend/local"
	"github.com/grafana/tempo/tempodb/backend/s3"
)

// adminStoreBucket adapts a tempodb backend reader to the bucket xstenant reads from.
type adminStoreBucket struct {
	r backend.RawReader
}

func (b adminStoreBucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	dir, file := path.Split(name)

	var keypath backend.KeyPath
	if dir = strings.Trim(dir, "/"); dir != "" {
		keypath = strings.Split(dir, "/")
	}

	rc, _, err := b.r.Read(ctx, file, keypath, nil)
	return rc, err
}

func (b adminStoreBucket) IsObjNotFoundErr(err error) bool {
	return errors.Is(err, backend.ErrDoesNotExist)
}

// newAdminStoreBucket builds a read-only view of the admin store bucket. The backends are
// created without their startup checks, which need permissions the engines do not have.
func newAdminStoreBucket(cfg *AdminStoreConfig) (adminStoreBucket, error) {
	var (
		r   backend.RawReader
		err error
	)

	switch cfg.Backend {
	case backend.Local:
		r, _, _, err = local.New(cfg.Local)
	case backend.GCS:
		r, _, _, err = gcs.NewNoConfirm(cfg.GCS)
	case backend.S3:
		r, _, _, err = s3.NewNoConfirm(cfg.S3)
	case backend.Azure:
		r, _, _, err = azure.NewNoConfirm(cfg.Azure)
	default:
		err = fmt.Errorf("unknown backend %q", cfg.Backend)
	}
	if err != nil {
		return adminStoreBucket{}, err
	}

	return adminStoreBucket{r: r}, nil
}
