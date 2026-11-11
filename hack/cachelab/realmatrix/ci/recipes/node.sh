# express: npm install, lint and mocha, as its CI. It has no lockfile.
IMAGE=node:24
ENV="npm_config_cache=$C/npm"
recipe() {
  step install "npm install --ignore-scripts --include=dev --no-audit --no-fund"
  step lint "npm run lint"
  step test "npm test"
}
