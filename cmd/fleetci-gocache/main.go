// Command fleetci-gocache is fleetci's shared Go build cache.
//
// With no arguments it is the go command's cache program, for
// GOCACHEPROG: it keeps the cache in FLEETCI_GOCACHE_DIR (default the user
// cache directory's fleetci-gocache), and with FLEETCI_GOCACHE_URL and
// FLEETCI_GOCACHE_TOKEN, which fleetci gives each job, shares it through
// the server.
//
//	fleetci-gocache serve -listen :8091 -public-key gocache.pub -s3-bucket b ...
//	fleetci-gocache keygen gocache.key gocache.pub
//	fleetci-gocache token -key gocache.key -project 42 -ref main -protected
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"github.com/nesty92/fleetci/internal/gocache"
	"github.com/nesty92/fleetci/internal/jobtoken"
)

func main() {
	var err error
	switch {
	case len(os.Args) < 2:
		err = client()
	case os.Args[1] == "serve":
		err = serve(os.Args[2:])
	case os.Args[1] == "keygen" && len(os.Args) == 4:
		err = jobtoken.GenerateKey(os.Args[2], os.Args[3])
	case os.Args[1] == "token":
		err = token(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: fleetci-gocache [serve flags... | keygen PRIVATE PUBLIC | token flags...]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetci-gocache:", err)
		os.Exit(1)
	}
}

func client() error {
	dir := os.Getenv("FLEETCI_GOCACHE_DIR")
	if dir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			d = os.TempDir()
		}
		dir = filepath.Join(d, "fleetci-gocache")
	}
	c := &gocache.Client{Dir: dir, URL: os.Getenv("FLEETCI_GOCACHE_URL"), Token: os.Getenv("FLEETCI_GOCACHE_TOKEN")}
	if os.Getenv("FLEETCI_GOCACHE_VERBOSE") != "" {
		c.Log = os.Stderr
	}
	return c.Serve(os.Stdin, os.Stdout)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", ":8091", "address to listen on")
	var pubs []string
	fs.Func("public-key", "a fleetci job token key (repeatable)", func(s string) error { pubs = append(pubs, s); return nil })
	dir := fs.String("dir", "", "keep entries in this directory instead of S3")
	var s3 gocache.S3Config
	fs.StringVar(&s3.Endpoint, "s3-endpoint", "", "S3 host[:port]; default AWS")
	fs.BoolVar(&s3.Insecure, "s3-insecure", false, "reach S3 over http")
	fs.StringVar(&s3.Region, "s3-region", "", "S3 region")
	fs.StringVar(&s3.Bucket, "s3-bucket", "", "S3 bucket")
	fs.StringVar(&s3.Prefix, "s3-prefix", "gocache/", "prefix of the objects")
	fs.StringVar(&s3.AccessKey, "s3-access-key", "", "S3 access key; default the AWS environment or the IAM role")
	secretFile := fs.String("s3-secret-key-file", "", "file holding the S3 secret key")
	_ = fs.Parse(args)

	log := hclog.New(&hclog.LoggerOptions{Name: "fleetci-gocache"})
	var keys []ed25519.PublicKey
	for _, p := range pubs {
		k, err := jobtoken.LoadPublicKey(p)
		if err != nil {
			return err
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return errors.New("serve: no -public-key")
	}
	var store gocache.Store = gocache.Dir(*dir)
	if *dir == "" {
		if *secretFile != "" {
			b, err := os.ReadFile(*secretFile)
			if err != nil {
				return err
			}
			s3.SecretKey = strings.TrimSpace(string(b))
		}
		st, err := gocache.NewS3(s3)
		if err != nil {
			return err
		}
		store = st
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           &gocache.Server{Store: store, Keys: keys, Log: log},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("serving", "listen", *listen)
	return srv.ListenAndServe()
}

// token prints a job token, to try a server without a forge.
func token(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	key := fs.String("key", "", "private key from keygen")
	var c jobtoken.Claims
	fs.StringVar(&c.Forge, "forge", "gitlab", "forge")
	fs.StringVar(&c.Instance, "instance", "gitlab.com", "forge host")
	fs.StringVar(&c.Project, "project", "", "project ID")
	fs.StringVar(&c.Ref, "ref", "", "branch or tag")
	fs.BoolVar(&c.Protected, "protected", false, "the ref is protected")
	fs.StringVar(&c.Job, "job", "0", "job ID")
	ttl := fs.Duration("ttl", time.Hour, "how long the token is valid")
	_ = fs.Parse(args)
	if *key == "" || c.Project == "" || c.Ref == "" {
		return errors.New("token: -key, -project and -ref are required")
	}
	k, err := jobtoken.LoadPrivateKey(*key)
	if err != nil {
		return err
	}
	c.Expires = time.Now().Add(*ttl).Unix()
	t, err := jobtoken.Sign(k, c)
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}
