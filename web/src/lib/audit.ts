import { pb } from "./pb";

export type AuditFilters = {
  q: string;
  window: "24h" | "7d" | "30d" | "all";
  actor: string;
  collection: string;
  host: string;
  action: string;
  record: string;
};

export const emptyFilters: AuditFilters = {
  q: "",
  window: "24h",
  actor: "",
  collection: "",
  host: "",
  action: "",
  record: "",
};

const windowHours: Record<string, number> = { "24h": 24, "7d": 24 * 7, "30d": 24 * 30 };

/** The boundary of a time window, as PocketBase stores dates. */
export function windowStart(window: AuditFilters["window"], now: number): string | null {
  const hours = windowHours[window];
  if (!hours) {
    return null;
  }
  return new Date(now - hours * 3600 * 1000).toISOString().replace("T", " ");
}

/** Builds the PocketBase filter of the audit tabs, with every value escaped. */
export function auditFilter(
  category: "change" | "security",
  filters: AuditFilters,
  now: number,
): string {
  const parts = [pb.filter("category = {:category}", { category })];

  const since = windowStart(filters.window, now);
  if (since) {
    parts.push(pb.filter("at >= {:since}", { since }));
  }
  if (filters.q) {
    parts.push(
      pb.filter("(record_label ~ {:q} || actor_label ~ {:q} || ip ~ {:q} || collection ~ {:q})", {
        q: filters.q,
      }),
    );
  }
  if (filters.actor) {
    parts.push(pb.filter("actor_label = {:actor}", { actor: filters.actor }));
  }
  if (filters.collection) {
    parts.push(pb.filter("collection = {:collection}", { collection: filters.collection }));
  }
  if (filters.host) {
    parts.push(pb.filter("host = {:host}", { host: filters.host }));
  }
  if (filters.action) {
    parts.push(pb.filter("action = {:action}", { action: filters.action }));
  }
  if (filters.record) {
    parts.push(pb.filter("record_id = {:record}", { record: filters.record }));
  }

  return parts.join(" && ");
}

export type DiffLine = { kind: "same" | "add" | "del"; text: string };

const maxDiffLines = 2000;

/** A line diff based on the longest common subsequence, for compose files and long text. */
export function lineDiff(before: string, after: string): DiffLine[] {
  const a = before.split("\n");
  const b = after.split("\n");

  if (a.length + b.length > maxDiffLines) {
    return [
      ...a.map((text): DiffLine => ({ kind: "del", text })),
      ...b.map((text): DiffLine => ({ kind: "add", text })),
    ];
  }

  const table: number[][] = Array.from({ length: a.length + 1 }, () =>
    new Array<number>(b.length + 1).fill(0),
  );
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      table[i][j] =
        a[i] === b[j] ? table[i + 1][j + 1] + 1 : Math.max(table[i + 1][j], table[i][j + 1]);
    }
  }

  const lines: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      lines.push({ kind: "same", text: a[i] });
      i++;
      j++;
    } else if (table[i + 1][j] >= table[i][j + 1]) {
      lines.push({ kind: "del", text: a[i] });
      i++;
    } else {
      lines.push({ kind: "add", text: b[j] });
      j++;
    }
  }
  for (; i < a.length; i++) {
    lines.push({ kind: "del", text: a[i] });
  }
  for (; j < b.length; j++) {
    lines.push({ kind: "add", text: b[j] });
  }

  return lines;
}

/** Shows unchanged lines only around changes, like a unified diff. */
export function withContext(lines: DiffLine[], context = 2): DiffLine[] {
  const keep = new Array<boolean>(lines.length).fill(false);

  lines.forEach((line, index) => {
    if (line.kind === "same") {
      return;
    }
    for (
      let k = Math.max(0, index - context);
      k <= Math.min(lines.length - 1, index + context);
      k++
    ) {
      keep[k] = true;
    }
  });

  return lines.filter((_, index) => keep[index]);
}

export function initials(label: string): string {
  const words = label.split(/[\s@._-]+/).filter(Boolean);
  if (words.length === 0) {
    return "?";
  }
  if (words.length === 1) {
    return words[0].slice(0, 2).toUpperCase();
  }
  return (words[0][0] + words[1][0]).toUpperCase();
}
