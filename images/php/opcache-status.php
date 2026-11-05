<?php

// Answers pocketcloud-php-status with OPcache's statistics. It must run in
// FPM: the command-line PHP has an OPcache of its own and would see nothing.
//
// It answers only a caller that knows the path of FPM's status page, which
// the entrypoint makes up anew at every start and which only the container
// knows (/run/pocketcloud-php/fpm-status.conf): the same secret that guards
// the status page itself. It does not ask fpm_get_status() which pool runs
// it, as that call sometimes crashes the worker of PHP 8.2's status pool.

$given = $_SERVER['POCKETCLOUD_STATUS_PATH'] ?? '';
$config = @file_get_contents('/run/pocketcloud-php/fpm-status.conf');
if (
    !is_string($given) || $given === '' || $config === false
    || !preg_match('/^pm\.status_path = (\S+)$/m', $config, $match)
    || !hash_equals($match[1], $given)
) {
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
