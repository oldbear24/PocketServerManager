/** One line of a followed log. The sequence number counts every line of a viewer. */
export type LogEntry = {
  seq: number;
  ts: string;
  container?: string;
  service?: string;
  stream?: string;
  text: string;
};

export type LogEvent =
  | { type: "lines"; lines: Omit<LogEntry, "seq">[] }
  | { type: "dropped"; count: number }
  | { type: "end"; error?: string };

/** A filter that cannot be compiled yet matches everything, while the user types. */
export function compileFilter(text: string): { regex: RegExp | null; invalid: boolean } {
  if (!text) {
    return { regex: null, invalid: false };
  }
  try {
    return { regex: new RegExp(text, "i"), invalid: false };
  } catch {
    return { regex: null, invalid: true };
  }
}

/** The lines a view shows, oldest first. */
export function visibleLines(
  lines: LogEntry[],
  options: { service?: string; regex: RegExp | null; upTo?: number | null },
): LogEntry[] {
  return lines.filter(
    (line) =>
      (!options.service || (line.service || line.container) === options.service) &&
      (options.upTo === undefined || options.upTo === null || line.seq <= options.upTo) &&
      (!options.regex || options.regex.test(line.text)),
  );
}

const levelPattern = /\b(FATAL|CRITICAL|ERROR|ERR|WARNING|WARN|NOTICE|INFO|DEBUG|LOG)\b/;

export type LevelTone = "err" | "warn" | "info";

/** Splits a line around the level word near its start, to colour only that word. */
export function splitLevel(text: string): { before: string; level: string; after: string } | null {
  const match = levelPattern.exec(text.slice(0, 80));
  if (!match) {
    return null;
  }
  return {
    before: text.slice(0, match.index),
    level: match[1],
    after: text.slice(match.index + match[1].length),
  };
}

export function levelTone(level: string): LevelTone {
  if (["FATAL", "CRITICAL", "ERROR", "ERR"].includes(level)) {
    return "err";
  }
  return level === "WARNING" || level === "WARN" ? "warn" : "info";
}

/** The file a download produces: the lines as they arrived, oldest first. */
export function logFileText(lines: LogEntry[]): string {
  return lines
    .map((line) => `${line.ts} ${line.service || line.container || "-"} ${line.text}`)
    .join("\n");
}
