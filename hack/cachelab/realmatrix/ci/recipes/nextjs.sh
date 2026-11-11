# vercel/chatbot: pnpm install, its checks and next build, with Next's
# build cache kept. The build needs its services' settings, not the
# services: placeholders do.
IMAGE=node:24
ENV="COREPACK_HOME=$C/corepack COREPACK_ENABLE_DOWNLOAD_PROMPT=0 NEXT_TELEMETRY_DISABLED=1"
recipe() {
  step install "corepack enable && pnpm install --frozen-lockfile --store-dir $C/pnpm-store"
  step lint "corepack enable && pnpm check"
  step build "corepack enable && mkdir -p $C/next .next && ln -sfn $C/next .next/cache &&
    AUTH_SECRET=placeholder POSTGRES_URL=postgres://ci@localhost/ci REDIS_URL=redis://localhost BLOB_READ_WRITE_TOKEN=placeholder pnpm exec next build"
}
