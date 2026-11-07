import type { RecordModel } from "pocketbase";

import { pb } from "./pb";

/** A static site on an edge server (#21). */
export type Site = RecordModel & {
  name: string;
  host: string;
  type: "static" | "php";
  source: SiteSource;
  repo_url: string;
  branch: string;
  generator: Generator;
  build_image: string;
  build_cmd: string;
  output_dir: string;
  build_cpus: number;
  build_memory_mb: number;
  keep_releases: number;
  https_redirect: boolean;
  hsts: boolean;
  compress: boolean;
  spa: boolean;
  not_found: string;
  headers: SiteHeader[] | null;
  cache_rules: CacheRule[] | null;
  /** the names of the users who may sign in; their hashes are never shown */
  basic_auth_users: string[] | null;
  git_token_set: boolean;
  active_deployment: string;
};

export type SiteSource = "git" | "upload" | "empty";

export type Generator = "auto" | "none" | "hugo" | "mkdocs" | "astro" | "npm" | "custom";

export const generators: Generator[] = ["auto", "none", "hugo", "mkdocs", "astro", "npm", "custom"];

export type SiteHeader = { path?: string; name: string; value: string };
export type CacheRule = { path: string; max_age: number };

/** One build and release of a site. */
export type Deployment = RecordModel & {
  site: string;
  number: number;
  status: "queued" | "building" | "succeeded" | "failed";
  trigger: "create" | "manual" | "push" | "upload";
  source: SiteSource;
  branch: string;
  commit_sha: string;
  message: string;
  requested_by: string;
  command: string;
  generator: string;
  image: string;
  build_cmd: string;
  started_at: string;
  finished_at: string;
  build_ms: number;
  size_bytes: number;
  files: number;
  error: string;
  /** the PHP version a PHP site's release was built for (#50) */
  php_version: string;
  /** its release is still on the server, so the site can go back to it */
  on_server: boolean;
  upload: string;
};

/** How a site's requests went over the last 24 hours, by the edge's
 * histogram of their durations (#50). */
export type SiteLatency = {
  requests: number;
  errors: number;
  p50_ms: number;
  p95_ms: number;
};

export type SiteRequests = {
  latency?: SiteLatency;
  /** the start of the first hour */
  from: string;
  /** requests in each of the last 24 hours, oldest first */
  hours: number[];
  total: number;
};

export const sitesApi = {
  deploy: (id: string) => pb.send<Deployment>(`/api/pc/sites/${id}/deploy`, { method: "POST" }),
  upload: (id: string, file: File) => {
    const body = new FormData();
    body.append("file", file);
    return pb.send<Deployment>(`/api/pc/sites/${id}/upload`, { method: "POST", body });
  },
  rollback: (id: string, deployment: string) =>
    pb.send<{ command: string }>(`/api/pc/sites/${id}/rollback`, {
      method: "POST",
      body: { deployment },
    }),
  credentials: (id: string, body: { git_token?: string; basic_auth?: string }) =>
    pb.send<Site>(`/api/pc/sites/${id}/credentials`, { method: "PUT", body }),
  webhook: (id: string) =>
    pb.send<{ url: string; secret: string }>(`/api/pc/sites/${id}/webhook`, {}),
  rotateWebhook: (id: string) =>
    pb.send<{ url: string; secret: string }>(`/api/pc/sites/${id}/webhook`, { method: "POST" }),
  requests: (id: string) => pb.send<SiteRequests>(`/api/pc/sites/${id}/requests`, {}),
};

/** A record a new site's names need, as the wizard shows it. */
export type PreviewRecord = { type: string; name: string; value: string; ttl: number };

/** What a new site's names need: the zone that holds them and its records. */
export type DnsPreview = {
  hostname: string;
  zone: string;
  /** the connected provider's name, empty when the records are made by hand */
  provider: string;
  kind: string;
  mode: "automatic" | "manual";
  records: PreviewRecord[];
  /** names that are domains already */
  taken: string[];
  problem?: string;
  /** a zone of a DNS server of the LAN: the certificate comes from the internal CA (#26) */
  lan?: boolean;
};

/** What the wizard sends for a new site (#24). */
export type ProvisionBody = {
  site: Record<string, unknown>;
  hostname: string;
  www: boolean;
  challenge: "auto" | "http-01" | "dns-01";
  /** a PHP site's runtime (#50) */
  php?: {
    version: string;
    docroot: string;
    template: string;
    database_server?: string;
    database?: string;
  };
};

/** The steps of a new site's setup, in order: each is a command. */
export const provisionSteps = [
  "site.create",
  "site.dns",
  "site.propagation",
  "db.database.create",
  "site.php.apply",
  "site.deploy",
  "site.certificate",
  "site.check",
] as const;

export const provisionApi = {
  /** creates the site and starts its setup; a site from an upload sends its ZIP */
  start: (body: ProvisionBody, file?: File) => {
    if (file) {
      const form = new FormData();
      form.append("data", JSON.stringify(body));
      form.append("file", file);
      return pb.send<{ provision: string; site: string }>("/api/pc/sites/provision", {
        method: "POST",
        body: form,
      });
    }
    return pb.send<{ provision: string; site: string }>("/api/pc/sites/provision", {
      method: "POST",
      body,
    });
  },
  retry: (id: string) => pb.send<void>(`/api/pc/provisions/${id}/retry`, { method: "POST" }),
  cancel: (id: string) => pb.send<void>(`/api/pc/commands/${id}/cancel`, { method: "POST" }),
  preview: (hostname: string, host: string, www: boolean) =>
    pb.send<DnsPreview>("/api/pc/dns/preview", {
      query: { hostname, host, www: String(www) },
      requestKey: "dns-preview",
    }),
};

/** The first seven characters of a commit, as Git shows it. */
export function shortCommit(sha: string): string {
  return sha.slice(0, 7);
}
