# flask: tox through uv, as its CI, on the image's Python: the tests and
# the type checks.
U="$RUN -e PIP_CACHE_DIR=/cache/pip -e UV_CACHE_DIR=/cache/uv -e UV_PYTHON_DOWNLOADS=never -e UV_LINK_MODE=copy python:3.13 sh -c"
pull python:3.13
step tox "$U 'pip install -q --root-user-action=ignore uv && uv run --locked --no-default-groups --group dev tox run -e py3.13,typing'"
