// Package cachelink is a bucket as a VM sees it: listings, and signed
// links to its objects. It imports no bucket's client, so what a VM runs
// stays small; internal/cachebucket is the bucket itself, on the nodes.
package cachelink

import (
	"context"
	"time"
)

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
