# rack: bundle install and rake, as its CI. The tests run as a user: some
# check file permissions, which root ignores.
IMAGE=ruby:4.0
ENV="BUNDLE_PATH=$C/bundle BUNDLE_JOBS=4"
recipe() {
  step install "bundle install"
  step_user test "bundle exec rake"
}
