package gocache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 keeps entries in an S3 bucket, each an object with its output ID and
// time as metadata.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
}

// S3Config is where and how to reach the bucket. Without keys, the AWS
// environment variables, then the instance's IAM role, are used.
type S3Config struct {
	Endpoint  string // host[:port]; default s3.amazonaws.com
	Insecure  bool   // http, not https
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
}

// NewS3 connects to the bucket.
func NewS3(c S3Config) (*S3, error) {
	if c.Bucket == "" {
		return nil, errors.New("s3: no bucket")
	}
	if c.Endpoint == "" {
		c.Endpoint = "s3.amazonaws.com"
	}
	creds := credentials.NewChainCredentials([]credentials.Provider{&credentials.EnvAWS{}, &credentials.IAM{}})
	if c.AccessKey != "" {
		creds = credentials.NewStaticV4(c.AccessKey, c.SecretKey, "")
	}
	client, err := minio.New(c.Endpoint, &minio.Options{Creds: creds, Secure: !c.Insecure, Region: c.Region})
	if err != nil {
		return nil, err
	}
	return &S3{client: client, bucket: c.Bucket, prefix: c.Prefix}, nil
}

const (
	metaOutput = "Output-Id"
	metaTime   = "Put-Time"
)

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, Entry, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.prefix+key, minio.GetObjectOptions{})
	if err != nil {
		return nil, Entry{}, err
	}
	info, err := obj.Stat()
	if err != nil {
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, Entry{}, errMiss
		}
		return nil, Entry{}, err
	}
	e := Entry{OutputID: info.UserMetadata[metaOutput], Size: info.Size, Time: info.LastModified}
	if ns, err := strconv.ParseInt(info.UserMetadata[metaTime], 10, 64); err == nil {
		e.Time = time.Unix(0, ns)
	}
	if !hexID.MatchString(e.OutputID) {
		obj.Close()
		return nil, Entry{}, fmt.Errorf("%s: no output ID", key)
	}
	return obj, e, nil
}

func (s *S3) Put(ctx context.Context, key string, e Entry, body io.Reader) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.prefix+key, body, e.Size, minio.PutObjectOptions{
		UserMetadata: map[string]string{metaOutput: e.OutputID, metaTime: strconv.FormatInt(e.Time.UnixNano(), 10)},
		ContentType:  "application/octet-stream",
	})
	return err
}

// Dir keeps entries in a directory: for tests, and a server on one host.
type Dir string

func (d Dir) path(key string) string { return filepath.Join(string(d), filepath.FromSlash(key)) }

func (d Dir) Get(_ context.Context, key string) (io.ReadCloser, Entry, error) {
	b, err := os.ReadFile(d.path(key) + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, Entry{}, errMiss
	}
	if err != nil {
		return nil, Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, Entry{}, err
	}
	f, err := os.Open(d.path(key))
	if err != nil {
		return nil, Entry{}, err
	}
	return f, e, nil
}

func (d Dir) Put(_ context.Context, key string, e Entry, body io.Reader) error {
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != e.Size {
		return fmt.Errorf("%s: got %d bytes, want %d", key, n, e.Size)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	b, _ := json.Marshal(e)
	return os.WriteFile(p+".json", b, 0o644)
}
