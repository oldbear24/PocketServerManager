import type { RecordModel } from "pocketbase";

import { pb } from "./pb";

/** A static site on an edge server (#21). */
export type Site = RecordModel & {
  name: string;
  host: string;
  type: "static";
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
  /** its release is still on the server, so the site can go back to it */
  on_server: boolean;
  upload: string;
};

export type SiteRequests = {
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

/** The first seven characters of a commit, as Git shows it. */
export function shortCommit(sha: string): string {
  return sha.slice(0, 7);
}
