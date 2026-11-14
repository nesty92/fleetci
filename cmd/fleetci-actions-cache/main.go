// fleetci-actions-cache is the cache server for actions/cache a job's VM runs (internal/actionscache).
// fleetci embeds it, for amd64 and arm64, and copies it into each VM that
// has a cache disk.
//
//	fleetci-actions-cache PORT UPSTREAM_URL STORE_DIR [any] [remote=off | remote=bucket node=URL]
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/nesty92/fleetci/internal/actionscache"
)

func main() {
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
