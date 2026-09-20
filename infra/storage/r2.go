package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"vozkot/domain/media"
	"vozkot/infra/config"
)

// R2 stores assets in a Cloudflare R2 bucket over the S3 API, the same access
// path Vozko's backend uses: static credentials, region "auto", and the S3
// endpoint derived from the account ID. Only the key prefix differs, and only
// because the two products share one bucket.
type R2 struct {
	client        *s3.Client
	bucket        string
	publicBaseURL string
	keyPrefix     string
}

var _ media.FileStorage = (*R2)(nil)

// NewR2 builds the client. Credentials are static and passed in, never read
// from the process environment here: configuration is loaded in one place and
// this constructor is given the result, which is what lets a test hand it
// something else.
func NewR2(ctx context.Context, cfg config.MediaConfig) (*R2, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, ""),
		),
		// R2 is single-region and rejects a real AWS region name.
		awsconfig.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("load R2 credentials: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID))
	})

	return &R2{
		client:        client,
		bucket:        cfg.Bucket,
		publicBaseURL: strings.TrimRight(cfg.PublicBaseURL, "/"),
		keyPrefix:     strings.Trim(cfg.KeyPrefix, "/"),
	}, nil
}

// Upload writes the object with its real Content-Type.
//
// The header is not cosmetic: an object stored without one is served as
// application/octet-stream, and a browser then downloads the file instead of
// rendering the image or playing the clip.
func (s *R2) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.object(key)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(resolveContentType(key, data, contentType)),
		// Assets are immutable: a new upload gets a new key, so the CDN may
		// hold them for as long as it likes.
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		return fmt.Errorf("upload %q to R2: %w", key, err)
	}
	return nil
}

// Delete removes the object. An object that is already gone is success: the
// caller wanted it absent, and it is.
func (s *R2) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.object(key)),
	})
	var missing *types.NoSuchKey
	if err != nil && !errors.As(err, &missing) {
		return fmt.Errorf("delete %q from R2: %w", key, err)
	}
	return nil
}

// URL is the public address of a key, built from the bucket's public hostname
// rather than the S3 endpoint, which is not publicly readable.
func (s *R2) URL(key string) string {
	return s.publicBaseURL + "/" + s.object(key)
}

// object is the bucket-side name of a logical key: the configured prefix, then
// the key the use case chose.
func (s *R2) object(key string) string {
	clean := strings.TrimLeft(key, "/")
	if s.keyPrefix == "" {
		return clean
	}
	return s.keyPrefix + "/" + clean
}

// resolveContentType picks the Content-Type the CDN will serve.
//
// Order matters: what the caller declared beats the key's extension, which
// beats sniffing, because only the caller knows the difference between formats
// that share their leading bytes.
//
// It never returns "", because an object stored without a type is served as
// application/octet-stream, which a browser downloads instead of rendering.
func resolveContentType(key string, data []byte, declared string) string {
	if ct := strings.TrimSpace(declared); ct != "" {
		return ct
	}
	if ext := path.Ext(key); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	if len(data) > 0 {
		// Reads at most the first 512 bytes, per net/http.
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}
