# excalidraw: yarn install, its checks, tests and the app's build.
IMAGE=node:24
ENV="COREPACK_HOME=$C/corepack COREPACK_ENABLE_DOWNLOAD_PROMPT=0 YARN_CACHE_FOLDER=$C/yarn"
recipe() {
  step install "corepack enable && yarn install --frozen-lockfile --network-timeout 600000"
  step types "corepack enable && yarn test:typecheck"
  step lint "corepack enable && yarn test:code"
  step test "corepack enable && yarn test:app --watch=false"
  step build "corepack enable && yarn build:app:docker"
}
