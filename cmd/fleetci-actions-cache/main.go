// fleetci-actions-cache is the cache server for actions/cache a job's VM runs (internal/actionscache).
// fleetci embeds it, for amd64 and arm64, and copies it into each VM that
// has a cache disk.
//
//	fleetci-actions-cache PORT UPSTREAM_URL STORE_DIR [any] [remote=off | remote=bucket node=URL]
//	fleetci-actions-cache gitlab PORT NODE_URL
//
// The second serves a GitLab job's cache from the per-file store, with
// the node's links (internal/gitlabcache).
//
// Run as tar (through a link named tar), it is the job's tar for
// actions/cache, which keeps its folders on the cache disk as folders
// (actionscache.TarMain).
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nesty92/fleetci/internal/actionscache"
	"github.com/nesty92/fleetci/internal/gitlabcache/vmcache"
)

func main() {
	if filepath.Base(os.Args[0]) == "tar" {
		os.Exit(actionscache.TarMain(os.Args[1:]))
	}
	if len(os.Args) == 4 && os.Args[1] == "gitlab" {
		os.Exit(gitlab(os.Args[2], os.Args[3]))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fleetci-actions-cache PORT UPSTREAM_URL STORE_DIR [any] [remote=off | remote=bucket node=URL]")
		os.Exit(2)
	}
	port, err := strconv.Atoi(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetci-actions-cache: port:", err)
		os.Exit(2)
	}
	cfg, err := actionscache.ParseArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleetci-actions-cache:", err)
		os.Exit(2)
	}
	cfg.Port = port
	lg := log.New(os.Stderr, "", log.Ltime)
	cfg.Log = lg
	srv := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", port),
		Handler:           actionscache.New(cfg),
		ReadHeaderTimeout: 30 * time.Second,
	}
	lg.Printf("serving on %d for %s store %s", port, cfg.Upstream, cfg.Store)
	lg.Fatal(srv.ListenAndServe())
}

// gitlab serves a GitLab job's cache: GitLab's helper restores and saves
// it here, and its files are kept in the runner's bucket, through the
// node's links (gitlabcache/vmcache).
func gitlab(port, node string) int {
	if _, err := strconv.Atoi(port); err != nil {
		fmt.Fprintln(os.Stderr, "fleetci-actions-cache gitlab: port:", err)
		return 2
	}
	lg := log.New(os.Stderr, "", log.Ltime)
	vm := &vmcache.VM{Node: node, Log: lg}
	srv := &http.Server{
		Addr: "0.0.0.0:" + port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !actionscache.Allowed(r) {
				http.Error(w, "only this VM", http.StatusForbidden)
				return
			}
			vm.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}
	lg.Printf("serving GitLab's cache on %s, links from %s", port, node)
	lg.Print(srv.ListenAndServe())
	return 1
}
