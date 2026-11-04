<?php

// Answers pocketcloud-php-status with OPcache's statistics. It must run in
// FPM: the command-line PHP has an OPcache of its own and would see nothing.
//
// It answers only in FPM's status pool, which listens on a Unix socket inside
// the container; through the site's pool on port 9000, which other containers
// reach, it does not exist.

$pool = function_exists('fpm_get_status') ? (fpm_get_status()['pool'] ?? '') : '';
if (!str_ends_with($pool, '_status')) {
    http_response_code(404);
    exit;
}

$opcache = function_exists('opcache_get_status') ? opcache_get_status(false) : false;

header('Content-Type: application/json');
header('Cache-Control: no-store');

echo json_encode(
    [
        'php_version' => PHP_VERSION,
        // false when OPcache is off for this pool
        'opcache' => $opcache === false ? ['opcache_enabled' => false] : $opcache,
    ],
    JSON_UNESCAPED_SLASHES | JSON_PRESERVE_ZERO_FRACTION | JSON_THROW_ON_ERROR,
);
