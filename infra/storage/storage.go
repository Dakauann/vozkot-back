// Package storage adapts Cloudflare R2 to the domain's FileStorage port.
//
// R2 is the only object store. It is reached over the S3 API with the same
// variable names, bucket and public hostname Vozko's backend uses, so one set
// of Cloudflare credentials serves both products and a key prefix keeps them
// out of each other's objects.
//
// There is deliberately no on-disk fallback. Bytes written to a container's
// filesystem do not survive the next deploy, while the URL stored on the media
// row does, so a local directory trades a loud failure at startup for a dead
// image weeks later. A missing credential fails here instead.
package storage

import (
	"context"

	"vozkot/domain/media"
	"vozkot/infra/config"
)

// New returns the object store every asset URL is served from. It fails when
// the configuration is incomplete: half-configured credentials would otherwise
// surface at the first upload, which is the worst moment to discover them.
func New(ctx context.Context, cfg config.MediaConfig) (media.FileStorage, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return NewR2(ctx, cfg)
}
