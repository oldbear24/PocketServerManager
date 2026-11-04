import { lineDiff } from "./audit";
import { pb } from "./pb";
import type { Command, Stack } from "./types";

export type StackProblem = { line?: number; column?: number; file?: string; message: string };

export type StackValidation = {
  valid: boolean;
  errors: StackProblem[];
  warnings: StackProblem[];
  services: string[];
};

export type ComposeProject = {
  project: string;
  services: string[];
  running: number;
  total: number;
  config_files: string[];
  working_dir: string;
};

export type SecretChange = { set?: Record<string, string>; remove?: string[] };

export type StackEdit = {
  compose_yaml?: string;
  env?: string;
  note?: string;
  secrets?: SecretChange;
};

export const stackApi = {
  validate: (body: { name: string; compose_yaml: string; env: string; secret_keys: string[] }) =>
    pb.send<StackValidation>("/api/pc/stacks/validate", { method: "POST", body }),

  save: (id: string, body: StackEdit) =>
    pb.send<Stack>(`/api/pc/stacks/${id}/save`, { method: "POST", body }),

  deploy: (id: string, body: StackEdit & { revision?: number; pull?: boolean }) =>
    pb.send<{ command: Command; stack: Stack }>(`/api/pc/stacks/${id}/deploy`, {
      method: "POST",
      body,
    }),

  rollback: (id: string, revision: number) =>
    pb.send<{ command: Command; stack: Stack }>(`/api/pc/stacks/${id}/rollback`, {
      method: "POST",
      body: { revision },
    }),

  composeProjects: (host: string) =>
    pb.send<ComposeProject[]>(`/api/pc/hosts/${host}/compose-projects`, {}),

  importProject: (host: string, project: string) =>
    pb.send<Command>(
      `/api/pc/hosts/${host}/compose-projects/${encodeURIComponent(project)}/import`,
      { method: "POST" },
    ),
};

/** The validation a rejected deploy carries, so that the editor can show it. */
export function rejectionOf(error: unknown): StackValidation | null {
  const response = (error as { response?: { validation?: StackValidation } } | null)?.response;
  return response?.validation ?? null;
}

/** How many lines were added and removed between two versions of a file. */
export function diffStats(before: string, after: string): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const line of lineDiff(before, after)) {
    if (line.kind === "add") {
      added++;
    } else if (line.kind === "del") {
      removed++;
    }
  }
  return { added, removed };
}

/** The numbers of the lines of `after` that `before` does not have, counted from 1. */
export function addedLines(before: string, after: string): number[] {
  const numbers: number[] = [];
  let current = 0;
  for (const line of lineDiff(before, after)) {
    if (line.kind !== "del") {
      current++;
    }
    if (line.kind === "add") {
      numbers.push(current);
    }
  }
  return numbers;
}

export type EnvEntry = { key: string; value: string };

/** The plain variables of an env file, for the overview next to the editor. */
export function envEntries(env: string): EnvEntry[] {
  const entries: EnvEntry[] = [];

  for (const raw of env.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) {
      continue;
    }
    const separator = line.indexOf("=");
    if (separator <= 0) {
      continue;
    }
    entries.push({
      key: line
        .slice(0, separator)
        .replace(/^export\s+/, "")
        .trim(),
      value: line
        .slice(separator + 1)
        .trim()
        .replace(/^(["'])(.*)\1$/, "$2"),
    });
  }

  return entries;
}

export const secretNamePattern = /^[A-Za-z_][A-Za-z0-9_]*$/;

export const stackNamePattern = /^[a-z0-9][a-z0-9_-]*$/;
