# The PHP runtime of PHP sites: the official php:<version>-fpm image with the
# extensions, tools and settings that sites expect. One image per PHP version
# in versions.json, built by .github/workflows/php.yml; see docs/architecture.md,
# "PHP sites".
#
#   docker build -f images/php/Dockerfile --build-arg PHP_VERSION=8.4 .

# the newest version in versions.json, and the Debian release named there
ARG PHP_VERSION=8.5
ARG DEBIAN_RELEASE=trixie
# CI pins the base by digest: php:<version>-fpm-<release>@sha256:…
ARG BASE_IMAGE=php:${PHP_VERSION}-fpm-${DEBIAN_RELEASE}

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS status
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/php-status ./cmd/php-status
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" \
    -o /out/pocketcloud-php-status ./cmd/php-status

FROM ${BASE_IMAGE}
ARG PHP_VERSION
ARG BASE_IMAGE
# the digest of the base, which CI compares with the official image's to know
# when to rebuild (.github/workflows/php.yml)
ARG BASE_DIGEST=

# Extensions from PECL and the tools, pinned and checked against these sums.
ARG REDIS_VERSION=6.3.0
ARG REDIS_SHA256=0d5141f634bd1db6c1ddcda053d25ecf2c4fc1c395430d534fd3f8d51dd7f0b5
ARG IMAGICK_VERSION=3.8.1
ARG IMAGICK_SHA256=3a3587c0a524c17d0dad9673a160b90cd776e836838474e173b549ed864352ee
ARG COMPOSER_VERSION=2.11.1
ARG COMPOSER_SHA256=6f1b891e1d4927d62740276c540fb490e47689e19e102c49e8da849794882f8c
ARG WP_CLI_VERSION=2.12.0
ARG WP_CLI_SHA512=be928f6b8ca1e8dfb9d2f4b75a13aa4aee0896f8a9a0a1c45cd5d2c98605e6172e6d014dda2e27f88c98befc16c040cbb2bd1bfa121510ea5cdf5f6a30fe8832
ARG WP_CLI_URL=https://github.com/wp-cli/wp-cli/releases/download/v${WP_CLI_VERSION}/wp-cli-${WP_CLI_VERSION}.phar

# The extensions, built against the headers of the libraries they need. Only
# those libraries stay, found with ldd as the official WordPress image does;
# the headers and the compilers of the base image ($PHPIZE_DEPS) go, so an image
# derived from this one that builds more extensions installs $PHPIZE_DEPS again.
RUN set -eux; \
    apt-get update; \
    apt-get upgrade -y; \
    savedAptMark="$(apt-mark showmanual)"; \
    apt-get install -y --no-install-recommends \
        libavif-dev \
        libfreetype-dev \
        libicu-dev \
        libjpeg62-turbo-dev \
        libmagickwand-dev \
        libpng-dev \
        libpq-dev \
        libwebp-dev \
        libzip-dev \
    ; \
    \
    docker-php-source extract; \
    curl -fsSL -o /tmp/redis.tgz "https://pecl.php.net/get/redis-${REDIS_VERSION}.tgz"; \
    echo "${REDIS_SHA256}  /tmp/redis.tgz" | sha256sum -c -; \
    mkdir /usr/src/php/ext/redis; \
    tar -xzf /tmp/redis.tgz -C /usr/src/php/ext/redis --strip-components=1; \
    curl -fsSL -o /tmp/imagick.tgz "https://pecl.php.net/get/imagick-${IMAGICK_VERSION}.tgz"; \
    echo "${IMAGICK_SHA256}  /tmp/imagick.tgz" | sha256sum -c -; \
    mkdir /usr/src/php/ext/imagick; \
    tar -xzf /tmp/imagick.tgz -C /usr/src/php/ext/imagick --strip-components=1; \
    rm /tmp/redis.tgz /tmp/imagick.tgz; \
    \
    docker-php-ext-configure gd --with-avif --with-freetype --with-jpeg --with-webp; \
    extensions="bcmath exif gd imagick intl mysqli pdo_mysql pdo_pgsql redis zip"; \
    # PHP 8.5 has OPcache built in; before, it is an extension of its own
    if ! php -m | grep -qx 'Zend OPcache'; then extensions="$extensions opcache"; fi; \
    docker-php-ext-install -j"$(nproc)" $extensions; \
    docker-php-source delete; \
    \
    apt-mark auto '.*' > /dev/null; \
    apt-mark manual $savedAptMark > /dev/null; \
    apt-mark auto $PHPIZE_DEPS libc6-dev > /dev/null; \
    extdir="$(php -r 'echo ini_get("extension_dir");')"; \
    ldd "$extdir"/*.so \
        | awk '/=>/ { so = $(NF-1); if (index(so, "/usr/local/") == 1) { next }; gsub("^/(usr/)?", "", so); printf "*%s\n", so }' \
        | sort -u \
        | xargs -r dpkg-query --search \
        | cut -d: -f1 \
        | sort -u \
        | xargs -rt apt-mark manual; \
    \
    apt-get install -y --no-install-recommends msmtp unzip; \
    apt-get purge -y --auto-remove -o APT::AutoRemove::RecommendsImportant=false; \
    rm -rf /var/lib/apt/lists/* /tmp/pear ~/.pearrc; \
    \
    # every extension still finds its libraries, and PHP starts without a warning
    if ldd "$extdir"/*.so | grep 'not found'; then exit 1; fi; \
    err="$(php --version 3>&1 1>&2 2>&3)"; \
    [ -z "$err" ]; \
    php -m

# Composer and WP-CLI. Composer raises PHP's memory limit by itself; WP-CLI
# does not, and 128 MB of php.ini are too few for `wp core download`, so wp
# runs it with the limit of WP_CLI_MEMORY_LIMIT. FPM keeps php.ini's.
RUN set -eux; \
    curl -fsSL -o /usr/local/bin/composer "https://getcomposer.org/download/${COMPOSER_VERSION}/composer.phar"; \
    echo "${COMPOSER_SHA256}  /usr/local/bin/composer" | sha256sum -c -; \
    mkdir -p /usr/local/lib/pocketcloud-php; \
    curl -fsSL -o /usr/local/lib/pocketcloud-php/wp-cli.phar "${WP_CLI_URL}"; \
    echo "${WP_CLI_SHA512}  /usr/local/lib/pocketcloud-php/wp-cli.phar" | sha512sum -c -; \
    printf '#!/bin/sh\nexec php -d memory_limit="${WP_CLI_MEMORY_LIMIT:-512M}" /usr/local/lib/pocketcloud-php/wp-cli.phar "$@"\n' > /usr/local/bin/wp; \
    chmod 0755 /usr/local/bin/composer /usr/local/bin/wp; \
    chmod 0644 /usr/local/lib/pocketcloud-php/wp-cli.phar

# Settings: php.ini-production with PocketCloud's own on top, FPM's pool, and
# the place where the entrypoint writes what changes at every start. msmtp
# reads its configuration from there through /etc/msmtprc.
RUN set -eux; \
    cp "$PHP_INI_DIR/php.ini-production" "$PHP_INI_DIR/php.ini"; \
    rm /usr/local/etc/php-fpm.d/*; \
    install -d -o www-data -g www-data -m 0700 /run/pocketcloud-php; \
    ln -s /run/pocketcloud-php/msmtprc /etc/msmtprc; \
    ln -s /usr/bin/msmtp /usr/sbin/sendmail
COPY images/php/php.ini $PHP_INI_DIR/conf.d/zz-pocketcloud.ini
COPY images/php/php-fpm.conf /usr/local/etc/php-fpm.d/www.conf
COPY images/php/opcache-status.php /usr/local/lib/pocketcloud-php/opcache-status.php
COPY --chmod=0755 images/php/entrypoint.sh /usr/local/bin/pocketcloud-php-entrypoint
COPY --from=status /out/pocketcloud-php-status /usr/local/bin/pocketcloud-php-status

# Caches of the tools in /tmp, as www-data's home is not its own.
ENV COMPOSER_HOME=/tmp/composer \
    WP_CLI_CACHE_DIR=/tmp/wp-cli-cache

LABEL org.opencontainers.image.title="pocketcloud-php" \
      org.opencontainers.image.description="PHP-FPM ${PHP_VERSION} for PocketCloud's PHP sites" \
      org.opencontainers.image.source="https://github.com/oldbear24/PocketServerManager" \
      org.opencontainers.image.version="${PHP_VERSION}" \
      org.opencontainers.image.base.name="${BASE_IMAGE}" \
      org.opencontainers.image.base.digest="${BASE_DIGEST}"

USER www-data
RUN php-fpm --test

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["pocketcloud-php-status", "-ping"]
ENTRYPOINT ["pocketcloud-php-entrypoint"]
CMD ["php-fpm"]
