# Cache disks

A fleetingd class with `cache_disk` gives each job's VM a disk that outlives
the VM. Docker's data is on it, so images the job pulled last time are still
there, and so are the archives of the job's own `cache:` (GitLab) and
`actions/cache` steps (GitHub): the disk is a local copy in front of the
cache the job already uses, S3 or GitHub's. Nothing changes in the job. The
README's "A cache disk per ref" section covers the setup; this page covers
how jobs use it, on GitLab and [on GitHub Actions](#github-actions).

- [How it works](#how-it-works)
- [Variables](#variables)
- [Recipes](#recipes): npm, pnpm, Yarn, Next.js, Turborepo, Playwright, Go,
  Rust, Python, PHP, Ruby, Java
- [Keys](#keys)
- [Things to know](#things-to-know)
- [Who writes the shared caches](#who-writes-the-shared-caches)
- [GitHub Actions](#github-actions)
- [For coding agents](#for-coding-agents): a block for AGENTS.md or CLAUDE.md

## How it works

1. **Each job has its own disk**, named by its ref and its job's name, a
   matrix job's values included: `main · build`, `feature/login · test
   (node 24)`. When a job starts, fleetci finds that disk's newest copy. If
   there is none yet, the job starts from the same job's disk on the
   default branch (or the first of `FLEETCI_CACHE_FROM` that has one), and
   with none of those, from an empty disk.
2. The job's VM gets a new copy-on-write layer on that disk. Docker's data
   is on it, and so is the job's cache directory.
3. When the job ends and the VM returns the disk cleanly, the layer becomes
   the disk's newest. A job that crashes leaves the disk as it was.

Jobs of one pipeline run at once, so they each get a disk: on one shared
disk, the last to finish would undo what the others added, archives and
Docker's data alike. A disk that starts from the default branch's shares
its blocks with it on the node, so the copy costs only what the job adds.

### The job's own caches

The job's `cache:` and `actions/cache` steps work as before. The disk sits
in front of where they store their archives:

- **GitLab:** the runner's cache directory (`/cache`, or its `cache_dir`)
  is on the disk. A `cache:` restore finds the archive there, as new as
  S3's copy, and skips the download. Uploads go to S3 as before, so jobs on
  other nodes and runners without a disk still get them.
- **GitHub:** an `actions/cache` restore asks fleetci's cache service in
  the VM. It asks GitHub which entry the key names, serves that entry from
  the disk if the disk has it, and otherwise downloads it from GitHub,
  in 8 MiB ranges, 8 at a time, keeping a copy. Saves are kept on the disk
  and go to GitHub as before. BuildKit's `type=gha` cache uses the same
  service.

The disk's copy of an archive is served only for the entry GitHub or S3
names now: a pull request's disk, which started from the default branch's,
can hold the same key of the default branch's scope, and that copy isn't
served for the pull request's own entry.

Each job's log ends with what its cache steps got, e.g.:

```
fleetci cache: 2 restored from the cache disk (412 MB), 1 saved, kept on the disk (38 MB)
```

Pulled images and service images are kept with nothing set.

## Variables

GitLab only, and seldom needed: `cache:` covers most jobs. They keep paths
on the disk without an archive, for tools whose caches are too large or too
many files to pack each time.

| Variable | Default | What it does |
|---|---|---|
| `FLEETCI_CACHE_PATHS` | none | Paths in the job's container to keep, separated by spaces or commas. Relative paths are in the project's checkout. |
| `FLEETCI_CACHE_KEY` | the ref and job | The disk's name. Jobs with the same key share a disk. |
| `FLEETCI_CACHE_FROM` | the default branch | Caches a key with nothing yet starts from, the first that exists. |
| `FLEETCI_CACHE` | on | `off` runs the job without a disk. |

The values expand GitLab variables: `FLEETCI_CACHE_KEY: $CI_JOB_NAME`.

## Recipes

Each recipe points the tool's cache at a fixed absolute path and keeps that
path. Keep a tool's download cache rather than its output where you can:
it is smaller, and the tool checks it against the lockfile, so a stale
cache can't change what gets built. `/fleetci/...` is only a convention. Any
path works, except `/cache`, which gitlab-runner uses for its own `cache:`.

### npm

```yaml
test:
  image: node:24
  variables:
    FLEETCI_CACHE_KEY: npm
    FLEETCI_CACHE_PATHS: /fleetci/npm
    npm_config_cache: /fleetci/npm
  script:
    - npm ci
    - npm test
```

`npm ci` deletes `node_modules` first, so keeping `node_modules` gains
nothing with it. With `npm install`, keep `node_modules` too.

### pnpm

```yaml
build:
  image: node:24
  variables:
    FLEETCI_CACHE_KEY: pnpm
    FLEETCI_CACHE_PATHS: /fleetci/pnpm-store /fleetci/corepack
    COREPACK_HOME: /fleetci/corepack
    COREPACK_ENABLE_DOWNLOAD_PROMPT: "0"
  script:
    - corepack enable
    - pnpm install --frozen-lockfile --store-dir /fleetci/pnpm-store
    - pnpm build
```

### Yarn (v1)

```yaml
test:
  image: node:24
  variables:
    FLEETCI_CACHE_KEY: yarn
    FLEETCI_CACHE_PATHS: /fleetci/yarn
    YARN_CACHE_FOLDER: /fleetci/yarn
  script:
    - yarn install --frozen-lockfile
    - yarn test
```

For Yarn 2 and later, set `YARN_GLOBAL_FOLDER` and `YARN_ENABLE_GLOBAL_CACHE: "true"` instead.

### Next.js

Keep the package store and `.next/cache`, where `next build` keeps its
compiled pages and optimised images between builds:

```yaml
build:
  image: node:24
  variables:
    FLEETCI_CACHE_KEY: next
    FLEETCI_CACHE_PATHS: /fleetci/pnpm-store .next/cache
    NEXT_TELEMETRY_DISABLED: "1"
  script:
    - corepack enable
    - pnpm install --frozen-lockfile --store-dir /fleetci/pnpm-store
    - pnpm exec next build
```

`.next/cache` is in the checkout. fleetci adds it to `GIT_CLEAN_FLAGS`, so
the checkout's `git clean` leaves it alone.

### Turborepo

```yaml
build:
  image: node:24
  variables:
    FLEETCI_CACHE_KEY: turbo
    FLEETCI_CACHE_PATHS: /fleetci/pnpm-store /fleetci/turbo
    TURBO_CACHE_DIR: /fleetci/turbo
  script:
    - corepack enable
    - pnpm install --frozen-lockfile --store-dir /fleetci/pnpm-store
    - pnpm turbo build
```

### Playwright

```yaml
e2e:
  image: node:24
  variables:
    FLEETCI_CACHE_KEY: playwright
    FLEETCI_CACHE_PATHS: /fleetci/npm /fleetci/ms-playwright
    npm_config_cache: /fleetci/npm
    PLAYWRIGHT_BROWSERS_PATH: /fleetci/ms-playwright
  script:
    - npm ci
    - npx playwright install --with-deps
    - npx playwright test
```

`install --with-deps` still installs the browsers' system packages each time.
To skip that too, use the `mcr.microsoft.com/playwright` image for your
Playwright version.

### Go

```yaml
test:
  image: golang:1.27
  variables:
    FLEETCI_CACHE_KEY: go
    FLEETCI_CACHE_PATHS: /fleetci/go-build /fleetci/go-mod
    GOCACHE: /fleetci/go-build
    GOMODCACHE: /fleetci/go-mod
  script:
    - go vet ./...
    - go test ./...
```

Go's build cache is keyed by content, so test results and compiled packages
come back only where the code is the same.

### Rust

```yaml
test:
  image: rust:1.96
  variables:
    FLEETCI_CACHE_KEY: cargo
    FLEETCI_CACHE_PATHS: /usr/local/cargo/registry /usr/local/cargo/git /fleetci/target
    CARGO_TARGET_DIR: /fleetci/target
    CARGO_INCREMENTAL: "0"
  script:
    - cargo build --workspace
    - cargo test --workspace
```

A `target` directory only grows. Past `max_per_cache` the key starts again
empty, so set that above a cold build's size.

### Python (pip, uv, Poetry)

```yaml
test:
  image: python:3.13
  variables:
    FLEETCI_CACHE_KEY: python
    FLEETCI_CACHE_PATHS: /fleetci/pip /fleetci/uv
    PIP_CACHE_DIR: /fleetci/pip
    UV_CACHE_DIR: /fleetci/uv
    UV_LINK_MODE: copy
  script:
    - pip install uv
    - uv sync --locked
    - uv run pytest
```

`UV_LINK_MODE: copy` stops uv warning that its cache and `.venv` are on
different filesystems. For Poetry, set `POETRY_CACHE_DIR: /fleetci/poetry`
and keep that path.

### PHP (Composer)

```yaml
test:
  image: composer:2
  variables:
    FLEETCI_CACHE_KEY: composer
    FLEETCI_CACHE_PATHS: /fleetci/composer
    COMPOSER_CACHE_DIR: /fleetci/composer
  script:
    - composer install --no-progress --no-interaction
    - vendor/bin/phpunit
```

### Ruby (Bundler)

```yaml
test:
  image: ruby:4.0
  variables:
    FLEETCI_CACHE_KEY: bundle
    FLEETCI_CACHE_PATHS: /fleetci/bundle
    BUNDLE_PATH: /fleetci/bundle
  script:
    - bundle install
    - bundle exec rake
```

### Java (Maven, Gradle)

```yaml
maven:
  image: maven:3-eclipse-temurin-21
  variables:
    FLEETCI_CACHE_KEY: maven
    FLEETCI_CACHE_PATHS: /fleetci/m2
  script:
    - mvn -B -ntp -Dmaven.repo.local=/fleetci/m2 verify

gradle:
  image: gradle:8-jdk21
  variables:
    FLEETCI_CACHE_KEY: gradle
    FLEETCI_CACHE_PATHS: /fleetci/gradle
    GRADLE_USER_HOME: /fleetci/gradle
  script:
    - gradle build --no-daemon
```

### Several jobs, several caches

Give jobs that keep different things different keys. Jobs that keep the
same things can share one:

```yaml
default:
  image: node:24
  before_script: [corepack enable]

variables:
  FLEETCI_CACHE_PATHS: /fleetci/pnpm-store
  FLEETCI_CACHE_KEY: pnpm

lint:
  script: [pnpm install --frozen-lockfile --store-dir /fleetci/pnpm-store, pnpm lint]

test:
  script: [pnpm install --frozen-lockfile --store-dir /fleetci/pnpm-store, pnpm test]

e2e:
  variables:
    FLEETCI_CACHE_KEY: e2e
    FLEETCI_CACHE_PATHS: /fleetci/pnpm-store /fleetci/ms-playwright
    PLAYWRIGHT_BROWSERS_PATH: /fleetci/ms-playwright
  script: [pnpm install --frozen-lockfile --store-dir /fleetci/pnpm-store, pnpm exec playwright install, pnpm e2e]

deploy:
  variables:
    FLEETCI_CACHE: "off"
  script: [./deploy.sh]
```

## Keys

- **The default is a disk per job.** Each job of each ref keeps its own,
  and its first run on a branch starts from the same job's on the default
  branch. That suits nearly every project.
- **A key of your own** (`FLEETCI_CACHE_KEY`) names the disk whole: jobs on
  the same key share it, run at once from the same layer, and the last one
  to finish wins: anything another job added is lost. Use it only for jobs
  that keep the same things and seldom run together.
- **Key the tool, not the lockfile.** Package managers check their caches
  against the lockfile, so a key that never changes keeps working through
  updates. A lockfile hash in the key gives every update an empty cache.
- **Starting points.** `FLEETCI_CACHE_FROM: develop main` makes a new disk
  start from `develop`'s if there is one, else from `main`'s.
- **Keys are per project.** No other project reads them.

## Things to know

- **One disk per job.** Every key, path and image of the job is on the same
  disk. Unlike stickydisk, a job doesn't get one disk per path.
- **Kept paths are mounted directories.** Docker creates one owned by root
  the first time. An image that runs as another user needs a step that
  chowns it, or a path that user owns.
- **Paths in the checkout** are excluded from its `git clean` with
  `-e /<path>` added to `GIT_CLEAN_FLAGS`. `GIT_CLEAN_FLAGS: none` turns
  cleaning off, as in GitLab. A path can't be the checkout or one of its
  parents.
- **Relative paths** follow `GIT_CLONE_PATH` and the runner's `builds_dir`.
  If `builds_dir` is mounted from the host, use absolute paths.
- **Sizes.** A disk past `max_per_cache` starts again empty. Disks no job
  has used for `max_age` (7 days) are deleted, a merge or pull request's
  sooner, after `reader_max_age` (2 days), and so are the least recently
  used when the class is over its `budget`.
- **Turning it off.** `FLEETCI_CACHE: "off"` gives a job no disk. Use it for
  a deploy job, or for a job that must start cold.
- **Emptying a key.** Change the key, e.g. `npm-2`. Its first job starts from
  `FLEETCI_CACHE_FROM`, or empty if that names nothing that exists.

## Who writes the shared caches

A job from a merge request must not change what the default branch's jobs run
with: a poisoned `node_modules`, package store or Docker image would ship. So
each project's keys have a shared cache, written only by its *writers'* jobs.
Every other job writes a cache of its own ref. That cache starts from the
shared one of the same key, and other refs never read it.

By default the writers are jobs on protected branches and tags. The fleetci
config, not the job, says otherwise:

| Rule | A job is a writer when it runs on |
|---|---|
| `protected` | a protected branch or tag (the default) |
| `default` | the default branch, protected or not |
| `branch:<glob>` | a branch that matches, `release/*` |
| `tag:<glob>` | a tag that matches, `v*` or `staging-*` |
| `all` | any branch, merge requests' too |

```toml
[gitlab.cache_disk]
writers = ["protected"]          # the default; [] makes no job a writer

# The first entry that matches the project applies.
[[gitlab.cache_disk.projects]]
match = "group/trunk-app"        # no protected branches; main deploys
writers = ["default"]

[[gitlab.cache_disk.projects]]
match = "group/services/**"      # main deploys to staging, tags to production
writers = ["default", "tag:v*"]
```

Merge request pipelines are never writers, except under `all`, or under
`protected` when GitLab protects the pipeline. fleetci decides from what
GitLab says of the job: its ref, whether that is a tag, whether it is
protected, and whether it is a merge request's. Variables the job sets don't
count, so a job can't make itself a writer by setting `CI_DEFAULT_BRANCH`.

**Don't use `all`** unless everyone who can push a branch is trusted with
what the default branch and the tags ship. It is there because some teams
work that way. It is not a good default.

## GitHub Actions

A GitHub job on a class with `cache_disk` gets a disk as a GitLab job does:
its own, by repo, branch and job name, with Docker's data and its
`actions/cache` archives on it. Nothing changes in the workflow.

GitHub gives a runner any queued job its labels fit, so fleetci learns
which job the VM serves only when the job starts. The runner runs a hook
before the job's first step. It tells fleetci the job's repo, ref and
whether the ref is protected, from the runner's own job context, then waits
while fleetci attaches the disk, which takes about a second, and prints the
outcome in the job's log:

```
Cache disk "main · build" (shared with the project)
Cache disk "feature/login · build" (this ref's own): starting from "main · build"'s
```

If fleetci doesn't answer within a minute, the job runs without a disk.

Who writes is decided as on GitLab, from what the job's runner says of it:
its branch, whether that branch is protected, and whether the run is a pull
request's. The rules are the same, in their own table:

```toml
[github.cache_disk]
writers = ["protected"]          # the default

[[github.cache_disk.projects]]
match = "my-org/trunk-app"       # owner/name, or "my-org/*"
writers = ["default"]
```

A pull request's job reads its base's shared disk and writes one of its
own; it writes the shared one only under `all`. A pull request from a fork
never writes, even under `all`, and its disk is kept apart, under the
fork's name.

fleetci's GitHub App needs only `Actions: read` and the permission to
register its self-hosted runners. It reads nothing of the repo's contents or
branch protection. The runner's image needs passwordless `sudo`, as for the
rest of fleetci.

### actions/cache

`actions/cache` (v4 and later, and anything on its toolkit, such as
`setup-node`'s `cache:` and BuildKit's `type=gha`) goes through fleetci's
cache service in the VM. The runner sets it up: fleetci points the
runner's `ACTIONS_RESULTS_URL` at the service, and the service talks to
GitHub's cache for anything not on the disk.

- **Job containers** (`container:`) reach the service through Docker's
  bridge. A host firewall that drops forwarded traffic, such as ufw's
  default, doesn't matter: the VM's own rules let it through.
- **Alpine job containers can't use `actions/cache`:** BusyBox's `tar`
  lacks what the action needs, with or without fleetci. Use a Debian-based
  image, e.g. `node:24-bookworm-slim`, or install GNU `tar`.
- **A runner fleetci couldn't patch** sends `actions/cache` to GitHub
  directly, as any runner does. The job's log says so in a warning, and the
  job still works.

The approach of pointing the runner at a cache service of one's own comes
from [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server)
(MIT), which serves `actions/cache` from its own storage. fleetci keeps
GitHub's cache behind the disk instead, so it needs no server of its own.

## On a fleet of machines

With controllers and workers (`fleetci controller`, `fleetci join`), a
class gets cache disks with `cache_disk = true` in the controllers'
`controller.toml`. Each worker keeps its own caches, under its `state_dir`,
and sizes them in its own `worker.toml`:

```toml
# controller.toml
[controller]
cache_wait = "30s"   # how long a job waits for the worker with its cache

[classes.4vcpu-8gb]
cpus = 4
memory = "8GB"
cache_disk = true

# worker.toml, on each worker
[cache_disk]
budget = "100GB"
min_free = "20GB"
max_per_cache = "30GB"
```

The workers report which caches they hold, and the leader boots a job's VM
on the worker that has the best one: the job's own key, then the shared
cache it falls back on, then `FLEETCI_CACHE_FROM`'s. If that worker is
full, the job waits up to `cache_wait` for room there, then goes to any
worker, where it starts from what that worker has, or empty. A GitHub job
is placed by what its queued job says; GitHub may give the runner another
job, which then starts from what that worker has.

The writers' rules are the controllers', as on one machine. Each job
writes its own worker's copy of a cache, and the workers then copy each
other's newer copies of the shared caches, those writers make: never a
merge request's or another ref's own cache.

- **Every 30 s** each worker asks the others it reaches (see
  [Neighbours](design/ha.md#neighbours-which-workers-reach-each-other))
  for their shared caches. It copies a newer copy of one it has, and one
  it lacks while its budget has room to spare: a copied cache never
  evicts one of its own.
- **Only the new layers move.** Layers never change once kept, so a
  newer copy costs the layers its jobs added since; a chain flattened
  on its worker comes over whole once.
- **Each layer is checked:** it comes over the workers' mutual TLS with
  its SHA-256 after it, and is kept only when they agree, and only once
  the whole chain has come. A job that starts meanwhile uses the copy
  its worker had.
- **Compressed:** a layer comes S2-compressed, at about half its size,
  with two cores of the sending worker.
- Two workers that run jobs of one cache at once each keep their own
  result, and the newer wins on both, as one machine's last job does.

```toml
# worker.toml
share_caches = false     # neither copy caches to others nor from them
share_compress = false   # copy them uncompressed: on links faster than 10 Gbit
peer_network = "eth1"    # or "192.168.10.0/24": copy over this network
```

Copies go over one network, by default the default route's, never one
that looks like a VPN (see
[Neighbours](design/ha.md#neighbours-which-workers-reach-each-other)).

A worker that runs any branch's jobs as writers (see the controllers'
`writers`) shares what they write too: one more reason to keep writers to
protected branches.

## For coding agents

Paste this into a project's `AGENTS.md` or `CLAUDE.md`, so coding agents
that edit `.gitlab-ci.yml` use the cache disk well:

````markdown
## CI cache disk (fleetci)

This project's GitLab jobs run on fleetci VMs with a cache disk of their
own. Pulled Docker images are kept with nothing set, and so are `cache:`
archives: the disk serves them before S3, so use `cache:` as usual. For a
tool cache too large to archive each time, keep its path on the disk:

- Point the tool's cache at an absolute path outside the checkout, and list
  that path in `FLEETCI_CACHE_PATHS` (space-separated):
  npm `npm_config_cache`, pnpm `--store-dir`, Yarn `YARN_CACHE_FOLDER`,
  Go `GOCACHE` and `GOMODCACHE`, Cargo `CARGO_TARGET_DIR` and
  `/usr/local/cargo/registry`, pip `PIP_CACHE_DIR`, uv `UV_CACHE_DIR`,
  Composer `COMPOSER_CACHE_DIR`, Bundler `BUNDLE_PATH`,
  Maven `-Dmaven.repo.local`, Gradle `GRADLE_USER_HOME`,
  Playwright `PLAYWRIGHT_BROWSERS_PATH`. Use `/fleetci/<tool>`; never
  `/cache`, which is gitlab-runner's.
- Each job already has its own disk. Don't set `FLEETCI_CACHE_KEY` unless
  several jobs keep the same things and seldom run together: jobs on one
  key overwrite each other's changes; the last to finish wins.
- Key by tool, not by lockfile hash. The tools check their caches against
  the lockfile already.
- Keep download caches rather than build output where both work. Keep
  `node_modules` only with `npm install`, never with `npm ci`.
- Set `FLEETCI_CACHE: "off"` on deploy and release jobs that must start
  clean.
- Don't try to make a merge request's jobs write the shared cache. fleetci
  decides that from GitLab's protected branches and its own config, not from
  job variables.
- A new cache starts from the default branch's (`FLEETCI_CACHE_FROM` to
  change it). The first run after changing a key is a cold run; that is
  expected.
- Job logs show which disk each job got: `Cache disk "main · test" (shared
  with the project)`, or `starting from "main · test"'s`. Check that line
  when a job is slower than expected.
````
