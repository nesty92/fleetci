# flask: tox through uv, as its CI, for one Python and the type checks.
IMAGE=python:3.13
ENV="PIP_CACHE_DIR=$C/pip UV_CACHE_DIR=$C/uv UV_PYTHON_DOWNLOADS=never UV_LINK_MODE=copy"
recipe() {
  step tox "pip install -q --root-user-action=ignore uv && uv run --locked --no-default-groups --group dev tox run -e py3.13,typing"
}
