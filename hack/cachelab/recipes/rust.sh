# ripgrep: cargo build and cargo test of the workspace, as its CI. The
# registry and target directory are on the cache disk, as rust-cache keeps
# them; incremental builds off, as CI has them.
R="$RUN -v /cache/cargo/registry:/usr/local/cargo/registry -v /cache/cargo/git:/usr/local/cargo/git -e CARGO_TARGET_DIR=/cache/cargo/target -e CARGO_INCREMENTAL=0 rust:1.96"
pull rust:1.96
step build "$R cargo build --workspace"
step test "$R cargo test --workspace"
