#!/bin/sh
# The smoke test of a PHP runtime image (#49): the extensions and tools are
# there, FPM runs as www-data with its status helper and health check, mail()
# goes to msmtp, and a WordPress and a Laravel skeleton app answer through
# Caddy's php_fastcgi, as a PHP site's edge route does.
#
#   images/php/smoke-test.sh ghcr.io/oldbear24/pocketcloud-php:8.4-fpm
#
# It needs Docker and the network: the apps are downloaded with WP-CLI and
# Composer from the image itself. SMOKE_FETCH_ARGS replace the network of the
# containers that download them, such as "--network host -e HTTPS_PROXY=…"
# behind a proxy; SMOKE_FETCH_IMAGE replaces the image of those containers and
# SMOKE_COMPOSER_ARGS add to Composer's, such as an image with git and
# --prefer-source where archives cannot be downloaded. Everything it starts is named pcsmoke-* and removed at the end.
set -eu

image=${1:?usage: smoke-test.sh IMAGE}
caddy=${SMOKE_CADDY:-docker.io/library/caddy:2.11.7}
mariadb=${SMOKE_MARIADB:-docker.io/library/mariadb:11.8}
net=pcsmoke-net
fetch_args=${SMOKE_FETCH_ARGS:---network $net}
fetch_image=${SMOKE_FETCH_IMAGE:-$image}
composer_args=${SMOKE_COMPOSER_ARGS:-}

say() { printf '\n== %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}

cleanup() {
	docker rm -f pcsmoke-fpm pcsmoke-caddy pcsmoke-db >/dev/null 2>&1 || true
	docker volume rm pcsmoke-sites >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

# run runs a one-off container of the image with the sites volume, as a site's
# build job does; fetch runs one that downloads.
run() {
	docker run --rm --network "$net" -v pcsmoke-sites:/srv/sites "$@"
}
fetch() {
	# shellcheck disable=SC2086 # fetch_args are words
	docker run --rm $fetch_args -v pcsmoke-sites:/srv/sites "$@"
}

say "extensions and tools"
modules=$(docker run --rm --entrypoint php "$image" -m)
for module in bcmath exif gd imagick intl mysqli pdo_mysql pdo_pgsql redis zip 'Zend OPcache'; do
	printf '%s\n' "$modules" | grep -qix "$module" || fail "PHP has no $module"
done
docker run --rm --entrypoint sh "$image" -ec '
	[ "$(id -un)" = www-data ]
	[ "$(readlink -f /usr/sbin/sendmail)" = /usr/bin/msmtp ]
	composer --version
	wp --version
	php -r "exit(ini_get(\"expose_php\") ? 1 : 0);"
	php -r "exit(function_exists(\"imagewebp\") && function_exists(\"imageavif\") ? 0 : 1);"
' || fail "a tool or setting is missing"

say "FPM, its status and its health"
docker network create "$net" >/dev/null
docker volume create pcsmoke-sites >/dev/null
# the volume belongs to www-data, as the sites volume's site directories do
docker run --rm -u 0 -v pcsmoke-sites:/srv/sites --entrypoint sh "$image" -ec 'mkdir -p /srv/sites/wp /srv/sites/laravel; chown -R www-data:www-data /srv/sites'

docker run -d --name pcsmoke-fpm --network "$net" --network-alias site-php \
	-v pcsmoke-sites:/srv/sites \
	-e POCKETCLOUD_SMTP_HOST=mail.invalid -e POCKETCLOUD_SMTP_USER=site -e POCKETCLOUD_SMTP_PASSWORD=secret \
	-e POCKETCLOUD_SMTP_FROM=noreply@example.org \
	--health-interval 2s "$image" >/dev/null

for _ in $(seq 60); do
	[ "$(docker inspect -f '{{.State.Health.Status}}' pcsmoke-fpm)" = healthy ] && break
	sleep 1
done
[ "$(docker inspect -f '{{.State.Health.Status}}' pcsmoke-fpm)" = healthy ] || fail "FPM never became healthy"

status=$(docker exec pcsmoke-fpm pocketcloud-php-status) || fail "the status helper failed: $status"
printf '%s\n' "$status" | grep -q '"fpm":{' || fail "no pool status: $status"
printf '%s\n' "$status" | grep -q '"opcache":{' || fail "no OPcache statistics: $status"
# cached_scripts counts the scripts in OPcache; the status script is one of them
cached_scripts() {
	docker exec pcsmoke-fpm pocketcloud-php-status | sed -n 's/.*"num_cached_scripts":\([0-9]*\).*/\1/p'
}
before=$(cached_scripts)
[ -n "$before" ] || fail "OPcache reports no count of cached scripts"
docker exec pcsmoke-fpm sh -ec '
	grep -q "^host mail.invalid$" /run/pocketcloud-php/msmtprc
	grep -qx "from \"noreply@example.org\"" /run/pocketcloud-php/msmtprc
	grep -qx "logfile /proc/1/fd/2" /run/pocketcloud-php/msmtprc
	[ "$(cat /run/pocketcloud-php/smtp-password)" = secret ]
' || fail "mail is not set up"
# the password is in a file of its own, never in the environment of FPM
if docker exec pcsmoke-fpm sh -c 'tr "\0" "\n" </proc/1/environ' | grep -q POCKETCLOUD_SMTP_PASSWORD; then
	fail "FPM's environment holds the SMTP password"
fi

say "the edge in front"
cat >/tmp/pcsmoke-Caddyfile <<'EOF'
{
	admin off
	auto_https off
}
:8080 {
	root * /srv/sites/wp
	php_fastcgi site-php:9000
	file_server
}
:8081 {
	root * /srv/sites/laravel/public
	php_fastcgi site-php:9000
	file_server
}
EOF
docker run -d --name pcsmoke-caddy --network "$net" -v pcsmoke-sites:/srv/sites:ro \
	-v /tmp/pcsmoke-Caddyfile:/etc/caddy/Caddyfile:ro "$caddy" >/dev/null
rm -f /tmp/pcsmoke-Caddyfile

# get asks the edge from inside the network, with the image's own curl
get() {
	docker run --rm --network "$net" --entrypoint curl "$image" -sS -o /dev/stdout -w '\n%{http_code}' "$@"
}

say "WordPress"
docker run -d --name pcsmoke-db --network "$net" -e MARIADB_ROOT_PASSWORD=root \
	-e MARIADB_DATABASE=wp -e MARIADB_USER=wp -e MARIADB_PASSWORD=wp "$mariadb" >/dev/null
fetch -w /srv/sites/wp "$fetch_image" wp core download --quiet
for _ in $(seq 60); do
	# over TCP as the site's user: during its first start, MariaDB answers on its
	# socket before it listens on the network
	docker exec pcsmoke-db mariadb -h127.0.0.1 -uwp -pwp wp -e 'SELECT 1' >/dev/null 2>&1 && break
	sleep 1
done
run -w /srv/sites/wp "$image" wp config create --dbname=wp --dbuser=wp --dbpass=wp --dbhost=pcsmoke-db --skip-check
run -w /srv/sites/wp "$image" wp core install --url=http://pcsmoke-caddy:8080 --title='PocketCloud smoke' \
	--admin_user=admin --admin_password=admin --admin_email=admin@example.com --skip-email
page=$(get http://pcsmoke-caddy:8080/)
[ "${page##*
}" = 200 ] || fail "WordPress answered ${page##*
}"
printf '%s\n' "$page" | grep -q 'PocketCloud smoke' || fail "WordPress did not render its title"

say "Laravel"
# shellcheck disable=SC2086 # composer_args are words
fetch -w /srv/sites "$fetch_image" composer create-project --no-interaction --no-progress $composer_args laravel/laravel laravel-app
run -w /srv/sites "$image" sh -ec 'cp -a laravel-app/. laravel/ && rm -rf laravel-app'
run -w /srv/sites/laravel "$image" php artisan --version
page=$(get http://pcsmoke-caddy:8081/)
[ "${page##*
}" = 200 ] || fail "Laravel answered ${page##*
}"
printf '%s\n' "$page" | grep -qi 'laravel' || fail "Laravel did not render its welcome page"

say "OPcache saw the apps"
# WordPress and Laravel each load dozens of files
after=$(cached_scripts)
[ "${after:-0}" -gt $((before + 50)) ] || fail "OPcache cached $before scripts before the apps and ${after:-none} after"
echo "OPcache cached $after scripts, $before before the apps"

echo
echo "OK: $image"
