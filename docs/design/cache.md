# Caches behind the cache disk

Status: the cache disk is built, with GitHub's cache or the GitLab runner's
S3 cache behind it, saved through (#18). On GitHub, `cache_store` also
puts the user's own bucket behind it, uploaded after the job, or nothing.
GitLab's side of both is open. This page keeps what the reference
projects and our own tests found, so it isn't learned twice.

The rule: a workflow written for GitHub's or GitLab's own runners runs
unchanged and only gets faster. Nothing a user has to add, rename or turn
on; `actions/cache`, `setup-*` caches, BuildKit's `type=gha` and GitLab's
`cache:` all land on the disk as they are.

## What the reference projects do

| Project | What it does | What fleetci takes |
|---|---|---|
| [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) (MIT, ideas, read at `a13ebc8`) | A server that speaks GitHub's cache protocol (v2, Twirp) from its own storage: a local folder, S3, GCS or Azure, with the metadata in SQLite, PostgreSQL or MySQL. Runners are pointed at it with a patched runner. Calls it doesn't serve, artifacts', go to GitHub | The protocol details below; the runner patch fleetci already applies (`[github] cache_url`); how to scope caches from the job's token |
| GitHub's cache service | Takes an entry's upload and finalize only while its job runs | Nothing can be saved to GitHub after the job: measured below |
| [useblacksmith/stickydisk](https://github.com/useblacksmith/stickydisk) | A disk per key and path, mounted into the job and committed after it | The disk that outlives the VM; fleetci's is one per job, with every cache step's paths on it |
| gitlab-runner (MIT) | `[runners.cache]`: the runner itself uploads `cache:` to S3, GCS or Azure after the job's script | GitLab's L2 stays as it is; the runner, which is fleetci, can upload after the job |

## What falcondev found

The server is about 11,000 lines of Rust; these are the parts a cache
behind fleetci's disk has to get right too.

### Scoping comes from the job's token

- `ACTIONS_RUNTIME_TOKEN` is a JWT. Its `ac` claim is a JSON list of
  `{"Scope": "refs/heads/main", "Permission": 3}`: bit 1 lets the job
  restore from that ref's caches, bit 2 lets it save to them. Its
  `repository_id` claim is the repo.
- A save goes to the first scope the job may write. A restore searches the
  scopes it may read by descending permission, so its own ref first, then
  the base and default branches.
- The token's signature is checked against the issuer's keys, found
  through OIDC discovery (`/.well-known/openid-configuration`, then its
  `jwks_uri`), cached 10 minutes and fetched again at most every 30 s for an
  unknown key. On GHES the issuer can carry an enterprise path while the
  keys sit at the host.
- So a cache of fleetci's own can apply exactly GitHub's rules, not a copy
  of them from the ref, and needs no GitHub API calls.

### Matching

GitHub's order, pinned by their tests
(`matches_scopes_then_keys_then_exact_before_prefix`):

1. Scopes in order. Within a scope, the primary key, then each restore key.
2. For each key, an exact match, else the newest entry it prefixes.
3. So the newest entry the primary key prefixes beats an exact restore key,
   and an exact restore key beats a newer entry it prefixes.
4. Keys compare byte for byte: case, accents and trailing spaces all count,
   and `%`, `_` and `\` in a restore key are literal, not patterns.

fleetci's `local_match` (internal/github/l1cache.py) follows this order,
with their cases as tests (`TestL1CacheDiskOnly`). It reads the scopes from
the job's own token without checking its signature: it runs inside the job
and only decides what the job's own disk answers. Entries filled from
GitHub carry no scope and come after the token's scopes.

### The protocol's corners

- **A save of a key that exists is a 409** (`already_exists`), not
  `ok: false`; that's what GitHub answers.
- **Uploads answer with `x-ms-request-id`.** Without it, BuildKit's client
  (`tonistiigi/go-actions-cache`, used by `cache-to: type=gha`)
  intermittently fails with EOF.
- **Block ids come in two shapes.** `@actions/cache` sends base64 of a
  36-character UUID and the block's index (48 bytes); buildx sends 64 bytes
  with a big-endian index at byte 16. Decoding accepts padded and unpadded,
  standard and URL-safe base64. An upload with no block id is one block.
- **The upload URL looks like Azure's** (`/devstoreaccount1/upload/<id>?sig=…`),
  so the Azure SDK in `@actions/cache` takes it. The `sig` is a random
  capability; only its hash is stored, and a block without it is a 403.
- **Calls come as JSON or as protobuf.** The request types carry a
  `metadata` field the server doesn't declare, which it must ignore.
  fleetci's service only reads JSON, which is what `@actions/cache` and
  BuildKit's client (`cache_v2.go`) send.
- **A download URL is a promise.** BuildKit fails the build on a 404 after
  a hit; it can't fall back to a miss. So before answering a hit, check the
  data is there, and drop the entry and match again if it isn't.
- **Ranges.** Serve `Range` on a whole object; clients go by the status
  (206 or 200), not `Accept-Ranges`. BuildKit reads at offsets through
  Azure's SDK, which sends `x-ms-range`; the disk's copies answer both.
- **Passthrough** forwards everything else byte for byte: drop hop-by-hop
  headers and `Accept-Encoding`, buffer the body so a 307 can replay it,
  and never log the query, which carries signatures.

### Storage

- Each block is stored as it comes; the entry becomes one object on its
  first download, or at finalize with `EAGER_MERGE`. On S3 that merge
  reads every byte back once. fleetci's in-VM service has the whole archive
  on the disk when the job saves, so it can upload one object and skip
  merging.
- Objects never change once written, so "it exists" is cached for a minute.
- Above 5 GiB an S3 upload must be multipart, in parts of 5 MiB or more.
  Unfinished multipart uploads are billed until aborted: a bucket rule with
  `AbortIncompleteMultipartUpload` after a day.
- Least permissions on S3: `s3:ListBucket` on the bucket; `s3:GetObject`,
  `s3:PutObject`, `s3:AbortMultipartUpload` and `s3:DeleteObject` on the
  prefix.

### Eviction and cleanup

- Last used is the last download, else the last save. Over the budget,
  evict least recently used until usage is 90 % of it; never an entry being
  read (a lease, renewed while the download runs).
- Entries unused for 90 days go; uploads idle for a minute go; data no
  entry points at goes after a grace period (24 h).
- Recency needs somewhere to record each download: a database. Without
  one, a bucket can only expire by age. fleetci's disks already keep
  recency per node, where it matters most.

### Their measurements

On one host (Ryzen 9 5950X, NVMe, filesystem storage): uploads of
8 × 128 MiB at 2.3 GiB/s, a first download at 7.9 GiB/s, 34,000 lookups a
second. On RustFS on the same host: uploads at about 1.1 GiB/s.

## What our tests found

### GitHub refuses a save after the job ends

`hack/cachelab/deferred`: a job created two entries and handed its token
and upload URLs to the lab's host, which uploaded and finalized them after
the job ended.

| After the job | Upload | Finalize |
|---|---|---|
| 1 min | 201 | 403 `permission_denied`: "workflow job run completed" |
| 10 min | 201 | 403, the same |

The token itself was still valid for about 6 hours. Neither entry was ever
listed. So GitHub's cache can only be written during the job, and a save to
it costs the job its upload time (about 100 s for 1 GB in the train).
GitLab's has no such limit: the runner uploads it.

## The user's bucket (`cache_store = "bucket"`)

Since GitHub's cache can't be written after the job, uploading after the
job needs a store fleetci writes itself. It is the user's bucket, set as
gitlab-runner's `[runners.cache]` and signed with gitlab-runner's own
adapters (`internal/cachebucket`; listing, which they don't do, is ours).

- **No server.** The VM's L1 (`l1cache.py`, `remote=bucket`) asks the node
  through the VM's SSH connection: a remote forward to the VM's loopback,
  set again when fleetci reconnects. Nothing new is reachable on any
  network, and the VM never holds the bucket's credentials.
- **The node checks the token** (`actionstoken.go`): the job's
  `ACTIONS_RUNTIME_TOKEN`, against the issuer's keys by OIDC discovery, as
  falcondev does. Its `ac` scopes say what a restore searches, in order,
  and where a save goes. The node lists the bucket under those scopes only.
- **Names keep prefixes.** Each part of
  `github/<repository_id>/<scope>/<version>/<key>` is escaped byte by byte
  outside `[A-Za-z0-9._~-]`, so a restore key's prefix is the object name's
  prefix, a scope's `/` can't reach a sibling scope, and keys compare byte
  for byte (`TestBucketMatching`, falcondev's cases).
- **Saves upload after the job.** A save is finalized on the disk during
  the job; once the runner exits, fleetci asks the L1 for `/fleetci/flush`,
  which gets an upload link from the node for each save (409 when the
  entry exists) and PUTs it. The job's token, valid for hours, is kept in
  the L1's memory only. The disk isn't detached until the flush is done or
  15 minutes pass; a save it misses stays on the disk.
- **Restores fill the disk** from the signed link in ranges, as from
  GitHub's cache. An entry whose object name the disk already has is
  served from the disk; if the node can't be reached, the disk answers
  alone.

Tested with a fake bucket (`TestL1CacheBucket`) and against RustFS with
real signed links (`TestL1CacheBucketS3`, `FLEETCI_TEST_S3`). GCS and
Azure share the code path except listing; their emulators can't check
signed links, which point at Google's and Azure's hosts. Cases
falcondev's suite has no counterpart for are in
`bucketcache_edge_test.go`: the node unreachable, an upload refused and
retried, a refused token, a key too long for a name, an object gone after
its listing, a ranged first read, parallel restores, objects not in
fleetci's layout, and a listing past one page.

A real workflow on the lab's two nodes, with RustFS as the bucket: its
first run saved 5 entries (380 MB), uploaded by the nodes after each job;
the next run, on fresh disks, restored them from the bucket (200 MB in
1.2 s, 100 MB in 0.2 s), and BuildKit's `type=gha` steps were `CACHED`.

## Open

- GitLab's side: a setting that turns the cache behind the disk off, and
  uploading after the job from the node (the runner uploads it during the
  job now).
