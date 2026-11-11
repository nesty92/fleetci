# excalidraw, a React app on Vite: yarn install, tsc, eslint, vitest and
# the app's build, as its CI.
Y="$RUN -e COREPACK_HOME=/cache/corepack -e COREPACK_ENABLE_DOWNLOAD_PROMPT=0 -e YARN_CACHE_FOLDER=/cache/yarn node:24 sh -c"
pull node:24
step install "$Y 'corepack enable && yarn install --frozen-lockfile --network-timeout 600000'"
step types "$Y 'corepack enable && yarn test:typecheck'"
step lint "$Y 'corepack enable && yarn test:code'"
step test "$Y 'corepack enable && yarn test:app --watch=false'"
step build "$Y 'corepack enable && yarn build:app:docker'"
