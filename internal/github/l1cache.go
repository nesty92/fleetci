package github

import _ "embed"

// l1CacheScript is the actions/cache L1 the VM runs (see l1cache.py): jobs
// restore and save through it, and their archives stay on the cache disk.
//
//go:embed l1cache.py
var l1CacheScript string

// l1CachePort is where l1cache.py listens on the VM.
const l1CachePort = 31849

// githubResultsURL is GitHub's cache service, the L2 when no cache_url is set.
const githubResultsURL = "https://results-receiver.actions.githubusercontent.com/"
