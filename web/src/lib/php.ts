import type { RecordModel } from "pocketbase";

import { pb } from "./pb";

/** Where a PHP version stands in its support, as internal/php says. */
export type PhpState = "supported" | "security" | "ending" | "ended";

/** A PHP version of the runtime images (images/php/versions.json) on today's date. */
export type PhpVersion = {
  version: string;
  image: string;
  released: string;
  active_support_until: string;
  security_support_until: string;
  state: PhpState;
  days_left: number;
};

export type PhpVersions = {
  warn_days: number;
  versions: PhpVersion[];
  /** what a new PHP site runs when it does not say */
  default: string;
};

export type PhpTemplate = "custom" | "wordpress" | "laravel" | "nette";

export type FpmSettings = {
  pm: "dynamic" | "ondemand" | "static";
  max_children: number;
  max_requests: number;
  request_terminate_timeout: number;
};

/** A PHP site's settings (#50): only the panel writes them, and applies each
 * change to the site's PHP-FPM container. */
export type SitePhp = RecordModel & {
  site: string;
  php_version: string;
  template: PhpTemplate;
  docroot: string;
  mode: "releases" | "classic";
  fpm: FpmSettings | null;
  php_ini: string;
  env: string;
  secret_names: string[] | null;
  cpu_limit: number;
  mem_limit_mb: number;
  shared_paths: string[] | null;
  release_hook: string;
  database: string;
  state: "pending" | "running" | "failed" | "";
  image: string;
  applied_at: string;
  last_error: string;
};

/** What may change of a PHP site's settings; what is left out stays. */
export type SitePhpChange = Partial<
  Pick<
    SitePhp,
    | "php_version"
    | "docroot"
    | "mode"
    | "template"
    | "php_ini"
    | "env"
    | "cpu_limit"
    | "mem_limit_mb"
    | "release_hook"
  >
> & { fpm?: FpmSettings; shared_paths?: string[] };

/** How a site's PHP-FPM is doing, as its server reads it. */
export type PhpStatus = {
  php_version: string;
  /** none when FPM's status page could not be read: errors say why */
  workers: {
    active: number;
    idle: number;
    total: number;
    max: number;
    max_active: number;
    max_children_reached: number;
    listen_queue: number;
    accepted: number;
    slow_requests: number;
    uptime: number;
  } | null;
  opcache: {
    enabled: boolean;
    hit_rate: number;
    used_bytes: number;
    free_bytes: number;
    cached_scripts: number;
    full: boolean;
  } | null;
  extensions: string[];
  shared: { path: string; bytes: number | null }[];
  errors?: string[];
  read_at: string;
};

/** The mail a PHP site sent through the panel's relay in the last day (#51). */
export type PhpMail = {
  relay: { host: string; port: number } | null;
  sent: number;
  failed: number;
  last_sent_at: string;
  failures: { at: string; recipients: string; error: string }[];
  /** Where the count starts: a day ago, or when PHP-FPM last started. */
  since: string;
};

/** A change applied to a site's PHP-FPM, or the deployment a new version takes. */
export type PhpApplied = { command?: string; deployment?: string; settings?: SitePhp };

export const phpApi = {
  versions: () => pb.send<PhpVersions>("/api/pc/php/versions", {}),
  settings: (site: string, change: SitePhpChange) =>
    pb.send<PhpApplied>(`/api/pc/sites/${site}/php`, { method: "PUT", body: change }),
  /** sets the site's secrets: a name with a value sets it, one with null
   * removes it, and those left out stay */
  secrets: (site: string, secrets: Record<string, string | null>) =>
    pb.send<PhpApplied & { secret_names: string[] }>(`/api/pc/sites/${site}/php/secrets`, {
      method: "PUT",
      body: secrets,
    }),
  apply: (site: string) =>
    pb.send<PhpApplied>(`/api/pc/sites/${site}/php/apply`, { method: "POST" }),
  mail: (site: string) =>
    pb.send<PhpMail>(`/api/pc/sites/${site}/php/mail`, { requestKey: `php-mail-${site}` }),
  status: (site: string) =>
    pb.send<PhpStatus>(`/api/pc/sites/${site}/php/status`, { requestKey: `php-status-${site}` }),
};

/** Whether a version is to be warned about: it ends soon or has ended. */
export function phpNeedsWarning(version: PhpVersion): boolean {
  return version.state === "ending" || version.state === "ended";
}
