<?php
/**
 * WordPress's settings, from the site's environment (PocketCloud, #52).
 *
 * The panel keeps them: the database it made for the site, the site's
 * addresses and a secret from which every key and salt is derived. Change
 * them on the site's page in PocketCloud rather than here. A source with a
 * wp-config.php of its own keeps it; this one is written anew at every
 * release while it carries the line below.
 *
 * @pocketcloud-template
 */

$pocketcloud_env = static function (string $name, string $default = ''): string {
	$value = getenv($name);
	return $value === false || $value === '' ? $default : $value;
};

define('DB_NAME', $pocketcloud_env('DB_NAME'));
define('DB_USER', $pocketcloud_env('DB_USER'));
define('DB_PASSWORD', $pocketcloud_env('DB_PASSWORD'));
define('DB_HOST', $pocketcloud_env('DB_HOST', 'localhost') . ($pocketcloud_env('DB_PORT') === '' ? '' : ':' . $pocketcloud_env('DB_PORT')));
define('DB_CHARSET', $pocketcloud_env('DB_CHARSET', 'utf8mb4'));
define('DB_COLLATE', '');

// a key or salt of the environment wins; the others come from WP_SALT
foreach (['AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY', 'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT'] as $pocketcloud_key) {
	define($pocketcloud_key, $pocketcloud_env($pocketcloud_key, hash_hmac('sha256', $pocketcloud_key, $pocketcloud_env('WP_SALT'))));
}

$table_prefix = $pocketcloud_env('WP_TABLE_PREFIX', 'wp_');

if ($pocketcloud_env('WP_HOME') !== '') {
	define('WP_HOME', $pocketcloud_env('WP_HOME'));
}
if ($pocketcloud_env('WP_SITEURL') !== '') {
	define('WP_SITEURL', $pocketcloud_env('WP_SITEURL'));
}

// In releases mode the code is read-only and every change to it is a new
// release: WordPress neither installs nor updates plugins, themes or itself,
// not even from its cron (WP-CLI does, in a release that is being built,
// which POCKETCLOUD_RELEASE says), and the panel runs its cron rather than
// its visitors.
$pocketcloud_releases = $pocketcloud_env('POCKETCLOUD_MODE', 'releases') === 'releases';
define('DISALLOW_FILE_MODS', $pocketcloud_releases && $pocketcloud_env('POCKETCLOUD_RELEASE') !== 'building');
define('AUTOMATIC_UPDATER_DISABLED', $pocketcloud_releases);
define('DISABLE_WP_CRON', $pocketcloud_releases);
define('FS_METHOD', 'direct');

define('WP_DEBUG', $pocketcloud_env('WP_DEBUG') === 'true');
define('WP_DEBUG_DISPLAY', false);
define('WP_ENVIRONMENT_TYPE', $pocketcloud_env('WP_ENVIRONMENT_TYPE', 'production'));

unset($pocketcloud_env, $pocketcloud_key, $pocketcloud_releases);

if (!defined('ABSPATH')) {
	define('ABSPATH', __DIR__ . '/');
}

require_once ABSPATH . 'wp-settings.php';
