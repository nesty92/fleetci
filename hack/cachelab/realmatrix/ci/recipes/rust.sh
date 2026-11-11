# ripgrep: cargo build and test of the workspace. The registry, git
# checkouts and target directory are kept.
IMAGE=rust:1.96
ENV="CARGO_HOME=$C/cargo CARGO_TARGET_DIR=$C/cargo/target CARGO_INCREMENTAL=0"
recipe() {
  step build "cargo build --workspace"
  step test "cargo test --workspace"
}
