# rack: bundle install and rake, as its CI, with the gems on the cache disk.
# The tests run as a user, as on CI: root reads an unreadable directory,
# which one test checks.
B="$RUN -e BUNDLE_PATH=/cache/bundle -e BUNDLE_JOBS=4"
pull ruby:4.0
step install "$B ruby:4.0 bundle install"
step test "chmod -R a+rwX /src && $B --user 1000:1000 -e HOME=/tmp ruby:4.0 bundle exec rake"
