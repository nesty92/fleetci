// Command release makes fleetci's signed release manifest, versions.json,
// in the release workflow, after GoReleaser:
//
//	go run ./hack/release keygen -o release.key    # once; prints the public key
//	go run ./hack/release manifest -version v0.7.0 -dist dist -out dist/release
//	go run ./hack/release verify -base https://github.com/nesty92/fleetci/releases/download/v0.7.0
//
// local signs a manifest of binaries in a directory, with any key, for a
// mirror or a test whose workers trust that key ([update] public_key):
//
//	go run ./hack/release local -version v0.7.0 -dir mirror/v0.7.0 -url http://mirror/v0.7.0 -key test.key
//
// manifest reads the secret key from $FLEETCI_RELEASE_KEY. It puts in
// fleetci from GoReleaser's dist, fleetingd from our fork's release
// (re-hosted on fleetci's, as a plain binary) and cloud-hypervisor from its
// own release, at the versions in deps.json, and writes what to upload to
// -out.
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesty92/fleetci/internal/cluster"
	"github.com/nesty92/fleetci/internal/release"
)

// deps are the versions of the binaries a release ships with fleetci.
type deps struct {
	CloudHypervisor string `json:"cloud-hypervisor"`
	Fleetingd       string `json:"fleeting-plugin-fleetingd"`
}

var arches = []string{"amd64", "arm64"}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: release keygen|manifest|verify|local [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "manifest":
		err = manifest(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "local":
		err = local(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("o", "release.key", "the secret key file to write")
	_ = fs.Parse(args)
	sk, err := release.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(sk.Marshal()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("wrote %s; the public key, for internal/release.PublicKey:\n%s\n", *out, sk.Public())
	return nil
}

func manifest(args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	version := fs.String("version", "", "the release's tag, e.g. v0.7.0")
	dist := fs.String("dist", "dist", "GoReleaser's output")
	depsFile := fs.String("deps", "hack/release/deps.json", "the versions of the other binaries")
	repo := fs.String("repo", "nesty92/fleetci", "fleetci's GitHub repository")
	fleetingd := fs.String("fleetingd-repo", "nesty92/fleeting-plugin-fleetingd", "fleetingd's")
	out := fs.String("out", "dist/release", "where to write what to upload")
	_ = fs.Parse(args)
	if *version == "" {
		return errors.New("-version is needed")
	}
	sk, err := release.ParseSecretKey(os.Getenv("FLEETCI_RELEASE_KEY"))
	if err != nil {
		return fmt.Errorf("$FLEETCI_RELEASE_KEY: %w", err)
	}
	if pk, _ := release.ParseKey(release.PublicKey); sk.Public().String() != pk.String() {
		return fmt.Errorf("$FLEETCI_RELEASE_KEY is key %s, and fleetci trusts %s", sk.Public().IDString(), pk.IDString())
	}
	var d deps
	b, err := os.ReadFile(*depsFile)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("%s: %w", *depsFile, err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	hc := &http.Client{}
	assets := "https://github.com/" + *repo + "/releases/download/" + *version + "/"
	m := release.Manifest{Version: *version, Protocol: cluster.Protocol, MinProtocol: cluster.MinProtocol, Platforms: map[string]map[string]release.Binary{}}
	for _, arch := range arches {
		bins := map[string]release.Binary{}
		name := "fleetci-linux-" + arch
		sum, size, err := release.SHA256File(filepath.Join(*dist, name))
		if err != nil {
			return err
		}
		bins["fleetci"] = release.Binary{Version: *version, URL: assets + name, SHA256: sum, Size: size}

		// fleetingd's release is a tarball; a machine gets the binary.
		name = "fleeting-plugin-fleetingd-linux-" + arch
		tgz := fmt.Sprintf("https://github.com/%s/releases/download/%s/fleeting-plugin-fleetingd_linux_%s.tar.gz", *fleetingd, d.Fleetingd, arch)
		if err := untarOne(ctx, hc, tgz, "fleeting-plugin-fleetingd", filepath.Join(*out, name)); err != nil {
			return err
		}
		if sum, size, err = release.SHA256File(filepath.Join(*out, name)); err != nil {
			return err
		}
		bins["fleeting-plugin-fleetingd"] = release.Binary{Version: d.Fleetingd, URL: assets + name, SHA256: sum, Size: size}

		// cloud-hypervisor's, from its own release: checked by its sum.
		chName := "cloud-hypervisor-static"
		if arch == "arm64" {
			chName += "-aarch64"
		}
		chURL := fmt.Sprintf("https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/%s/%s", d.CloudHypervisor, chName)
		tmp := filepath.Join(*out, ".ch-"+arch)
		if err := fetch(ctx, hc, chURL, tmp); err != nil {
			return err
		}
		sum, size, err = release.SHA256File(tmp)
		os.Remove(tmp)
		if err != nil {
			return err
		}
		bins["cloud-hypervisor"] = release.Binary{Version: d.CloudHypervisor, URL: chURL, SHA256: sum, Size: size}
		m.Platforms["linux/"+arch] = bins
	}
	data, sig, err := release.SignManifest(sk, m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, release.ManifestName), data, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*out, release.ManifestName+".minisig"), sig, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s signed with key %s:\n%s", release.ManifestName, sk.Public().IDString(), data)
	return nil
}

func local(args []string) error {
	fs := flag.NewFlagSet("local", flag.ExitOnError)
	version := fs.String("version", "", "the release's version, e.g. v0.7.0")
	dir := fs.String("dir", "", "the binaries: fleetci, fleeting-plugin-fleetingd, cloud-hypervisor; the manifest goes here too")
	url := fs.String("url", "", "where -dir is served")
	keyFile := fs.String("key", "", "the secret key to sign with")
	arch := fs.String("arch", "amd64", "the binaries' GOARCH")
	_ = fs.Parse(args)
	if *version == "" || *dir == "" || *url == "" || *keyFile == "" {
		return errors.New("-version, -dir, -url and -key are needed")
	}
	b, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	sk, err := release.ParseSecretKey(string(b))
	if err != nil {
		return fmt.Errorf("%s: %w", *keyFile, err)
	}
	bins := map[string]release.Binary{}
	for _, name := range []string{"fleetci", "fleeting-plugin-fleetingd", "cloud-hypervisor"} {
		sum, size, err := release.SHA256File(filepath.Join(*dir, name))
		if err != nil {
			return err
		}
		bins[name] = release.Binary{Version: *version, URL: strings.TrimSuffix(*url, "/") + "/" + name, SHA256: sum, Size: size}
	}
	m := release.Manifest{Version: *version, Protocol: cluster.Protocol, MinProtocol: cluster.MinProtocol,
		Platforms: map[string]map[string]release.Binary{"linux/" + *arch: bins}}
	data, sig, err := release.SignManifest(sk, m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, release.ManifestName), data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, release.ManifestName+".minisig"), sig, 0o644)
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	base := fs.String("base", "", "the release's asset URL, or a local directory")
	_ = fs.Parse(args)
	k, err := release.ParseKey(release.PublicKey)
	if err != nil {
		return err
	}
	var m release.Manifest
	if strings.HasPrefix(*base, "https://") || strings.HasPrefix(*base, "http://") {
		m, err = release.FetchManifest(context.Background(), http.DefaultClient, k, *base)
	} else {
		var data, sig []byte
		if data, err = os.ReadFile(filepath.Join(*base, release.ManifestName)); err == nil {
			if sig, err = os.ReadFile(filepath.Join(*base, release.ManifestName+".minisig")); err == nil {
				m, err = release.ParseManifest(k, data, sig)
			}
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("ok    %s, protocol %d (from %d), signed with key %s\n", m.Version, m.Protocol, m.MinProtocol, k.IDString())
	return nil
}

func fetch(ctx context.Context, hc *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// untarOne fetches a .tar.gz and writes its member to path.
func untarOne(ctx context.Context, hc *http.Client, url, member, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			return fmt.Errorf("%s: no %s in it: %w", url, member, err)
		}
		if filepath.Clean(h.Name) != member || h.Typeflag != tar.TypeReg {
			continue
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, 1<<30)); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
}
