# zustand: pnpm install, then its CI's checks and build.
IMAGE=node:24
ENV="COREPACK_HOME=$C/corepack COREPACK_ENABLE_DOWNLOAD_PROMPT=0"
recipe() {
  step install "corepack enable && pnpm install --frozen-lockfile --store-dir $C/pnpm-store"
  step format "corepack enable && pnpm run test:format"
  step types "corepack enable && pnpm run test:types"
  step lint "corepack enable && pnpm run test:lint"
  step spec "corepack enable && pnpm run test:spec"
  step build "corepack enable && pnpm run build"
}
