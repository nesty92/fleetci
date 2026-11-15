// Package cachebucket is a cache's store in a bucket of the user's own:
// S3, GCS or Azure, configured as a GitLab runner's [runners.cache] is.
// Nodes list it and sign links to its objects; VMs only ever get the links.
package cachebucket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gitlab.com/gitlab-org/gitlab-runner/cache"
	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
	// The adapters register themselves by type.
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/azure"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/gcs"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/gcsv2"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/s3"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/s3v2"
)

// Config is a GitLab runner's [runners.cache]: Type is s3, s3v2, gcs,
// gcsv2 or azure, with its own table.
type Config = cacheconfig.Config

// Object is one object of a listing.
type Object struct {
	Name     string
	Size     int64
	Modified time.Time
}

// Link is a signed URL and the headers a request to it must send.
type Link struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Bucket lists objects and signs links to them. Names are relative to
// the configured Path.
type Bucket interface {
	// List is every object whose name starts with prefix.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Download is a link to read the object, for ttl.
	Download(ctx context.Context, name string, ttl time.Duration) (Link, error)
	// Upload is a link to write the object, for ttl.
	Upload(ctx context.Context, name string, ttl time.Duration) (Link, error)
}

// Validate says what is missing from cfg.
func Validate(cfg *Config) error {
	if cfg == nil || cfg.Type == "" {
		return errors.New("no Type")
	}
	switch cfg.Type {
	case "s3", "s3v2":
		if cfg.S3 == nil || cfg.S3.BucketName == "" {
			return fmt.Errorf("type %s: no [s3] BucketName", cfg.Type)
		}
	case "gcs", "gcsv2":
		if cfg.GCS == nil || cfg.GCS.BucketName == "" {
			return fmt.Errorf("type %s: no [gcs] BucketName", cfg.Type)
		}
	case "azure":
		if cfg.Azure == nil || cfg.Azure.ContainerName == "" || cfg.Azure.AccountName == "" {
			return fmt.Errorf("type azure: no [azure] AccountName and ContainerName")
		}
	default:
		return fmt.Errorf("type %q: want s3, s3v2, gcs, gcsv2 or azure", cfg.Type)
	}
	return nil
}

// New is the bucket cfg names.
func New(cfg *Config) (Bucket, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	var l lister
	var closer func()
	var err error
	switch cfg.Type {
	case "s3", "s3v2":
		l, closer, err = newS3Lister(cfg.S3)
	case "gcs", "gcsv2":
		l, closer, err = newGCSLister(cfg.GCS)
	case "azure":
		l, closer, err = newAzureLister(cfg.Azure)
	}
	if err != nil {
		return nil, fmt.Errorf("cache bucket %s: %w", cfg.Type, err)
	}
	return &bucket{cfg: cfg, list: l, closer: closer, prefix: pathPrefix(cfg.Path)}, nil
}

type lister func(ctx context.Context, prefix string) ([]Object, error)

type bucket struct {
	cfg    *Config
	list   lister
	closer func()
	prefix string
}

// Close lets go of the bucket's connections.
func (b *bucket) Close() {
	if b.closer != nil {
		b.closer()
	}
}

func pathPrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func (b *bucket) List(ctx context.Context, prefix string) ([]Object, error) {
	objs, err := b.list(ctx, b.prefix+prefix)
	if err != nil {
		return nil, err
	}
	for i := range objs {
		objs[i].Name = strings.TrimPrefix(objs[i].Name, b.prefix)
	}
	return objs, nil
}

func (b *bucket) Download(ctx context.Context, name string, ttl time.Duration) (Link, error) {
	a, err := b.adapter(name, ttl)
	if err != nil {
		return Link{}, err
	}
	return link(a.GetDownloadURL(ctx))
}

func (b *bucket) Upload(ctx context.Context, name string, ttl time.Duration) (Link, error) {
	a, err := b.adapter(name, ttl)
	if err != nil {
		return Link{}, err
	}
	l, err := link(a.GetUploadURL(ctx))
	if err == nil {
		// The adapters sign the archiver's Content-Type.
		if l.Headers == nil {
			l.Headers = map[string]string{}
		}
		if _, ok := l.Headers[http.CanonicalHeaderKey("Content-Type")]; !ok {
			l.Headers["Content-Type"] = cache.UploadContentType
		}
	}
	return l, err
}

func (b *bucket) adapter(name string, ttl time.Duration) (cache.Adapter, error) {
	create, err := cache.Factories().Find(b.cfg.Type)
	if err != nil {
		return nil, err
	}
	return create(b.cfg, ttl, b.prefix+name)
}

func link(p cache.PresignedURL) (Link, error) {
	if p.URL == nil {
		return Link{}, errors.New("the cache bucket signed no URL")
	}
	l := Link{URL: p.URL.String()}
	for k, v := range p.Headers {
		if len(v) > 0 {
			if l.Headers == nil {
				l.Headers = map[string]string{}
			}
			l.Headers[http.CanonicalHeaderKey(k)] = v[0]
		}
	}
	return l, nil
}
