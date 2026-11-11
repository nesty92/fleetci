# commonmark: composer update and phpunit, as its CI. Composer's cache
# keeps the packages.
IMAGE=composer:2
ENV="COMPOSER_CACHE_DIR=$C/composer"
recipe() {
  step install "composer update --no-progress --no-interaction"
  step phpunit "php vendor/bin/phpunit --no-coverage"
}
