# vercel/chatbot, a Next.js app: pnpm install, its lint (ultracite) and
# next build, as its CI and a deploy run them. Its build script migrates a
# database first, which is left out; the build gets placeholder secrets.
# .next/cache is on the cache disk, as a GitLab cache: entry for it would
# keep it there.
P="$RUN -e COREPACK_HOME=/cache/corepack -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 -e NEXT_TELEMETRY_DISABLED=1 node:24 sh -c"
pull node:24
step install "$P 'corepack enable && pnpm install --frozen-lockfile --store-dir /cache/pnpm-store'"
step lint "$P 'corepack enable && pnpm check'"
step build "$P 'corepack enable && mkdir -p /cache/next .next && ln -sfn /cache/next .next/cache &&
  AUTH_SECRET=placeholder POSTGRES_URL=postgres://ci@localhost/ci REDIS_URL=redis://localhost BLOB_READ_WRITE_TOKEN=placeholder pnpm exec next build'"
