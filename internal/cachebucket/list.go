package cachebucket

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"gitlab.com/gitlab-org/gitlab-runner/cache/cacheconfig"
)

// Listing is the one thing gitlab-runner's adapters don't do: they sign
// links to one object. These list with the same settings.

// defaultS3Endpoint and fallbackRegion are gitlab-runner's (cache/s3v2).
const (
	defaultS3Endpoint = "https://s3.amazonaws.com"
	fallbackRegion    = "us-east-1"
)

// newS3Lister is gitlab-runner's newRawS3Client (cache/s3v2/s3.go; MIT, see
// LICENSE.gitlab-runner), fleetci: without detecting the bucket's region,
// which needs s3:GetBucketLocation; BucketLocation or AWS_REGION say it.
func newS3Lister(c *cacheconfig.CacheS3Config) (lister, func(), error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	opts := []func(*config.LoadOptions) error{config.WithHTTPClient(&http.Client{Transport: tr})}
	if c.AuthType() == cacheconfig.S3AuthTypeAccessKey {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.SessionToken)))
	}
	region := c.BucketLocation
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	if region == "" {
		region = fallbackRegion
	}
	opts = append(opts, config.WithRegion(region))
	endpoint := c.GetEndpoint()
	custom := endpoint != "" && endpoint != defaultS3Endpoint
	if custom {
		if os.Getenv("AWS_RESPONSE_CHECKSUM_VALIDATION") == "" {
			opts = append(opts, config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired))
		}
		if os.Getenv("AWS_REQUEST_CHECKSUM_CALCULATION") == "" {
			opts = append(opts, config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if custom {
			o.BaseEndpoint = aws.String(endpoint)
		} else {
			o.UseDualstack = c.DualStackEnabled() // nolint:staticcheck
			o.UseAccelerate = c.Accelerate
		}
		o.UsePathStyle = c.PathStyleEnabled()
	})
	bucket := c.BucketName
	return func(ctx context.Context, prefix string) ([]Object, error) {
		var out []Object
		p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, o := range page.Contents {
				out = append(out, Object{Name: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
			}
		}
		return out, nil
	}, tr.CloseIdleConnections, nil
}

// newGCSLister lists as gitlab-runner's gcsv2 adapter authenticates.
func newGCSLister(c *cacheconfig.CacheGCSConfig) (lister, func(), error) {
	var opts []option.ClientOption
	if c.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(c.CredentialsFile)) // nolint:staticcheck
	}
	if c.UniverseDomain != "" {
		opts = append(opts, option.WithUniverseDomain(c.UniverseDomain))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, nil, err
	}
	b := client.Bucket(c.BucketName)
	return func(ctx context.Context, prefix string) ([]Object, error) {
		var out []Object
		it := b.Objects(ctx, &storage.Query{Prefix: prefix})
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, Object{Name: a.Name, Size: a.Size, Modified: a.Updated})
		}
	}, func() { _ = client.Close() }, nil
}

// newAzureLister lists as gitlab-runner's azure adapter authenticates: the
// account key, else Azure's default credential (a managed identity).
func newAzureLister(c *cacheconfig.CacheAzureConfig) (lister, func(), error) {
	domain := c.StorageDomain
	if domain == "" {
		domain = "blob.core.windows.net"
	}
	u := fmt.Sprintf("https://%s.%s/%s", c.AccountName, domain, c.ContainerName)
	var client *container.Client
	if c.AccountKey != "" {
		cred, err := azblob.NewSharedKeyCredential(c.AccountName, c.AccountKey)
		if err != nil {
			return nil, nil, err
		}
		client, err = container.NewClientWithSharedKeyCredential(u, cred, nil)
		if err != nil {
			return nil, nil, err
		}
	} else {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, nil, err
		}
		client, err = container.NewClient(u, cred, nil)
		if err != nil {
			return nil, nil, err
		}
	}
	return func(ctx context.Context, prefix string) ([]Object, error) {
		var out []Object
		p := client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
		for p.More() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, b := range page.Segment.BlobItems {
				o := Object{Name: *b.Name}
				if b.Properties != nil {
					if b.Properties.ContentLength != nil {
						o.Size = *b.Properties.ContentLength
					}
					if b.Properties.LastModified != nil {
						o.Modified = *b.Properties.LastModified
					}
				}
				out = append(out, o)
			}
		}
		return out, nil
	}, nil, nil
}
