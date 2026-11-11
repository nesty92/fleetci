# zustand: pnpm install, prettier, tsc, eslint, vitest and the build, as
# its CI. Corepack fetches the pnpm its package.json names.
P="$RUN -e COREPACK_HOME=/cache/corepack -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 node:24 sh -c"
pull node:24
step install "$P 'corepack enable && pnpm install --frozen-lockfile --store-dir /cache/pnpm-store'"
step format "$P 'corepack enable && pnpm run test:format'"
step types "$P 'corepack enable && pnpm run test:types'"
step lint "$P 'corepack enable && pnpm run test:lint'"
step spec "$P 'corepack enable && pnpm run test:spec'"
step build "$P 'corepack enable && pnpm run build'"
