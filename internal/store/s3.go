// Package store reads objects from S3 and mints presigned URLs for them.
//
// It exists because the two ways to get a large video into Meta are to stream
// the bytes yourself or to hand Meta a URL it can fetch. When the asset already
// lives in S3, a presigned URL is much the cheaper option.
package store

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Ref is a parsed s3://bucket/key reference.
type Ref struct {
	Bucket string
	Key    string
}

func (r Ref) String() string { return "s3://" + r.Bucket + "/" + r.Key }

// IsRef reports whether a string is an s3:// URI.
func IsRef(s string) bool { return strings.HasPrefix(s, "s3://") }

// ParseRef splits an s3://bucket/key URI.
func ParseRef(s string) (Ref, error) {
	if !IsRef(s) {
		return Ref{}, fmt.Errorf("not an s3 uri: %q", s)
	}
	u, err := url.Parse(s)
	if err != nil {
		return Ref{}, fmt.Errorf("parse %q: %w", s, err)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if u.Host == "" || key == "" {
		return Ref{}, fmt.Errorf("s3 uri needs both bucket and key: %q", s)
	}
	return Ref{Bucket: u.Host, Key: key}, nil
}

// Client wraps the S3 API surface this tool needs. The underlying AWS client is
// built on first use, so nothing about AWS is loaded for invocations that never
// touch S3.
type Client struct {
	Region string

	once sync.Once
	api  *s3.Client
	err  error
}

// New returns a lazily-initialised S3 client using the default AWS credential
// chain -- environment, shared config, SSO, instance role.
func New(region string) *Client { return &Client{Region: region} }

func (c *Client) s3(ctx context.Context) (*s3.Client, error) {
	c.once.Do(func() {
		var opts []func(*awsconfig.LoadOptions) error
		if c.Region != "" {
			opts = append(opts, awsconfig.WithRegion(c.Region))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			c.err = fmt.Errorf("load aws config: %w", err)
			return
		}
		c.api = s3.NewFromConfig(cfg)
	})
	return c.api, c.err
}

// Open returns a reader over an S3 object along with its size.
func (c *Client) Open(ctx context.Context, ref Ref) (io.ReadCloser, int64, error) {
	api, err := c.s3(ctx)
	if err != nil {
		return nil, 0, err
	}
	out, err := api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &ref.Bucket,
		Key:    &ref.Key,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get %s: %w", ref, err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// Presign returns a time-limited URL that Meta can fetch the object from.
func (c *Client) Presign(ctx context.Context, ref Ref, ttl time.Duration) (string, error) {
	api, err := c.s3(ctx)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	ps := s3.NewPresignClient(api, func(o *s3.PresignOptions) { o.Expires = ttl })
	req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &ref.Bucket,
		Key:    &ref.Key,
	})
	if err != nil {
		return "", fmt.Errorf("presign %s: %w", ref, err)
	}
	return req.URL, nil
}
