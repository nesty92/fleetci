# Cache disks

A fleetingd class with `cache_disk` gives each job's VM a disk that outlives
the VM. Docker's data is on it, so images the job pulled last time are still
there, and so are the paths the job asks to keep. The README's "A cache disk
per ref" section covers the setup; this page covers using it from
`.gitlab-ci.yml`, and [from GitHub Actions](#github-actions).

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

1. When a job starts, fleetci finds the newest disk for the job's key. The
   key is the job's ref unless the job names one. If that key has no disk
   yet, fleetci uses the first of `FLEETCI_CACHE_FROM` that has one; that is
   the default branch's unless the job lists others. If none exists, the job
   starts with an empty disk.
2. The job's VM gets a new copy-on-write layer on that disk. Docker uses it,
   and each path in `FLEETCI_CACHE_PATHS` is a directory on it, mounted into
   the job's container.
3. When the job ends and the VM returns the disk cleanly, the layer becomes
   the key's newest. A job that crashes leaves the key as it was.

Pulled images and service images are kept with no variables set. The
variables are for everything else.

## Variables

| Variable | Default | What it does |
|---|---|---|
| `FLEETCI_CACHE_PATHS` | none | Paths in the job's container to keep, separated by spaces or commas. Relative paths are in the project's checkout. |
| `FLEETCI_CACHE_KEY` | the ref | The cache's name. Jobs with the same key share a cache. |
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

- **The default key is the ref.** Each branch then keeps its own cache, and
  its first job starts from the default branch's. That suits a project with
  one kind of job.
- **One key per kind of job.** Use `npm`, `cargo`, `e2e` or `$CI_JOB_NAME`
  when a pipeline's jobs keep different things. Jobs on the same key run at
  once from the same layer, and the last one to finish wins: anything another
  job added is lost.
- **Key the tool, not the lockfile.** Package managers check their caches
  against the lockfile, so a key that never changes keeps working through
  updates. A lockfile hash in the key gives every update an empty cache.
- **Starting points.** `FLEETCI_CACHE_FROM: develop main` makes a new key
  start from `develop`'s cache if there is one, else from `main`'s.
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
- **Sizes.** A key past `max_per_cache` starts again empty. Keys no job has
  used for `max_age` are deleted, and so are the least recently used keys
  when the class is over its `budget`.
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

A GitHub job on a class with `cache_disk` gets a disk keyed by its repo and
branch, with Docker's data on it: images and service containers pulled by
the branch's last job are there. Nothing changes in the workflow.

GitHub gives a runner any queued job its labels fit, so fleetci learns
which repo and branch the VM serves only when the job starts. The runner
runs a hook before the job's first step. The hook waits while fleetci looks
the job up on GitHub's API and attaches its disk, which takes a second or
two, then prints the outcome in the job's log:

```
Cache disk "main" (shared with the project)
Cache disk "feature/login" (this ref's own): starting from "main"'s
```

If fleetci doesn't answer within a minute, the job runs without a disk.

Who writes is decided as on GitLab, from what GitHub says of the job: its
branch, whether that branch is protected, and whether the run is a pull
request's. The rules are the same, in their own table:

```toml
[github.cache_disk]
writers = ["protected"]          # the default

[[github.cache_disk.projects]]
match = "my-org/trunk-app"       # owner/name, or "my-org/*"
writers = ["default"]
```

A pull request's job reads its base's shared cache and writes one of its
own; it writes the shared one only under `all`. A pull request from a fork
never writes, even under `all`, and its cache is kept apart, under the
fork's name.

fleetci's GitHub App or token reads the job and its run, which needs
`Actions: read` on the repos it serves. Rules other than `default` and
`all` also need the branch, to tell a protected branch or a tag: that
needs `Contents: read`. Without it, the job's log says so and the job runs
with no disk. The runner's image needs passwordless `sudo`, as for the
rest of fleetci.

Not yet on GitHub: choosing keys and paths to keep (the `FLEETCI_CACHE_*`
variables are GitLab's), and serving `actions/cache` from the disk.

## For coding agents

Paste this into a project's `AGENTS.md` or `CLAUDE.md`, so coding agents
that edit `.gitlab-ci.yml` use the cache disk well:

````markdown
## CI cache disk (fleetci)

This project's GitLab jobs run on fleetci VMs with a cache disk. Pulled
Docker images are kept with nothing set. To keep a tool's cache, set
variables on the job; there are no `cache:` uploads to wait for.

- Point the tool's cache at an absolute path outside the checkout, and list
  that path in `FLEETCI_CACHE_PATHS` (space-separated):
  npm `npm_config_cache`, pnpm `--store-dir`, Yarn `YARN_CACHE_FOLDER`,
  Go `GOCACHE` and `GOMODCACHE`, Cargo `CARGO_TARGET_DIR` and
  `/usr/local/cargo/registry`, pip `PIP_CACHE_DIR`, uv `UV_CACHE_DIR`,
  Composer `COMPOSER_CACHE_DIR`, Bundler `BUNDLE_PATH`,
  Maven `-Dmaven.repo.local`, Gradle `GRADLE_USER_HOME`,
  Playwright `PLAYWRIGHT_BROWSERS_PATH`. Use `/fleetci/<tool>`; never
  `/cache`, which is gitlab-runner's.
- Give each kind of job its own `FLEETCI_CACHE_KEY` (`npm`, `cargo`,
  `e2e`). Jobs on one key overwrite each other's changes; the last to finish
  wins.
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
- Job logs show which cache each job got: `Cache disk "npm" (shared with
  the project)`, or `starting from "main"'s`. Check that line when a job is
  slower than expected.
````
