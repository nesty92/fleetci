# express: npm install, lint and mocha, as its CI. It has no lockfile.
N="$RUN -e npm_config_cache=/cache/npm node:24"
pull node:24
step install "$N npm install --ignore-scripts --include=dev --no-audit --no-fund"
step lint "$N npm run lint"
step test "$N npm test"
