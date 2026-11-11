# league/commonmark: composer update and phpunit, as its CI. Its CI runs
# phpcs and phpstan on PHP 7.4, where the versions composer picks pass;
# on this image's PHP they pick newer ones that find new issues.
C="$RUN -e COMPOSER_CACHE_DIR=/cache/composer composer:2"
pull composer:2
step install "$C composer update --no-progress --no-interaction"
step phpunit "$C php vendor/bin/phpunit --no-coverage"
