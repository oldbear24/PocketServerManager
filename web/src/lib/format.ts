import i18n from "../i18n";

function locale() {
  return i18n.language || "en";
}

/** PocketBase stores dates as "2026-10-16 13:42:10.000Z". */
export function parseDate(value: string | null | undefined): Date | null {
  if (!value) {
    return null;
  }
  const date = new Date(value.replace(" ", "T"));
  return Number.isNaN(date.getTime()) ? null : date;
}

export function formatDateTime(value: string | null | undefined): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  return new Intl.DateTimeFormat(locale(), { dateStyle: "medium", timeStyle: "medium" }).format(
    date,
  );
}

export function formatDate(value: string | null | undefined): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  return new Intl.DateTimeFormat(locale(), { dateStyle: "medium" }).format(date);
}

/**
 * A calendar day such as "2026-12-31", which names no time and no zone. It is
 * read and shown as that day in UTC, so that west of UTC it is not the day before.
 */
export function formatCalendarDate(value: string | null | undefined): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  return new Intl.DateTimeFormat(locale(), { dateStyle: "medium", timeZone: "UTC" }).format(date);
}

export function formatTime(
  value: string | null | undefined,
  options: { milliseconds?: boolean } = {},
): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  return new Intl.DateTimeFormat(locale(), {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    fractionalSecondDigits: options.milliseconds ? 3 : undefined,
    hourCycle: "h23",
  }).format(date);
}

export function formatDay(value: string | null | undefined): string {
  const date = parseDate(value);
  if (!date) {
    return "–";
  }
  return new Intl.DateTimeFormat(locale(), {
    weekday: "long",
    day: "numeric",
    month: "numeric",
    year: "numeric",
  }).format(date);
}

/** "4 s ago", "in 12 days": the largest unit that fits. */
export function formatRelative(
  value: string | Date | null | undefined,
  now = Date.now(),
  options: { past?: boolean } = {},
): string {
  const date = value instanceof Date ? value : parseDate(value);
  if (!date) {
    return "–";
  }

  let seconds = Math.round((date.getTime() - now) / 1000);
  // A moment that has passed, such as a heartbeat, can look a little ahead of a
  // clock that ticks every few seconds or runs behind the panel's.
  if (options.past && seconds > 0) {
    seconds = 0;
  }
  const absolute = Math.abs(seconds);
  const format = new Intl.RelativeTimeFormat(locale(), { numeric: "auto", style: "short" });

  if (absolute < 60) {
    return format.format(seconds, "second");
  }
  if (absolute < 3600) {
    return format.format(Math.round(seconds / 60), "minute");
  }
  if (absolute < 86400) {
    return format.format(Math.round(seconds / 3600), "hour");
  }
  return format.format(Math.round(seconds / 86400), "day");
}

export function formatDuration(seconds: number): string {
  if (!seconds || seconds < 0) {
    return "–";
  }
  if (seconds < 60) {
    return `${Math.round(seconds)} s`;
  }
  if (seconds < 3600) {
    return `${Math.round(seconds / 60)} min`;
  }
  if (seconds < 86400) {
    return `${Math.round(seconds / 3600)} h`;
  }
  return `${Math.round(seconds / 86400)} d`;
}

export function formatBytes(value: number | null | undefined): string {
  if (!value || value <= 0) {
    return "–";
  }
  const units = ["B", "kB", "MB", "GB", "TB", "PB"];
  const exponent = Math.min(Math.floor(Math.log10(value) / 3), units.length - 1);
  const scaled = value / 1000 ** exponent;
  const digits = scaled >= 100 || exponent === 0 ? 0 : 1;
  return `${new Intl.NumberFormat(locale(), { maximumFractionDigits: digits }).format(scaled)} ${units[exponent]}`;
}

/** Traffic the way it is usually talked about: bits per second, from bytes per second. */
export function formatBitrate(bytesPerSecond: number | null | undefined): string {
  if (bytesPerSecond === null || bytesPerSecond === undefined || bytesPerSecond < 0) {
    return "–";
  }
  const bits = bytesPerSecond * 8;
  const units = ["b/s", "kb/s", "Mb/s", "Gb/s", "Tb/s"];
  const exponent = bits < 1 ? 0 : Math.min(Math.floor(Math.log10(bits) / 3), units.length - 1);
  const scaled = bits / 1000 ** exponent;
  const digits = scaled >= 100 || exponent === 0 ? 0 : 1;
  return `${new Intl.NumberFormat(locale(), { maximumFractionDigits: digits }).format(scaled)} ${units[exponent]}`;
}

/** A moment on a chart's axis: the time for up to a day, the date for a week. */
export function formatChartTime(seconds: number, range: string): string {
  const date = new Date(seconds * 1000);
  if (range === "7d") {
    return new Intl.DateTimeFormat(locale(), {
      day: "numeric",
      month: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    }).format(date);
  }
  return new Intl.DateTimeFormat(locale(), {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).format(date);
}

/** A label of a time axis whose ticks are `step` seconds apart: a date for whole days. */
export function formatAxisTime(seconds: number, step: number): string {
  const date = new Date(seconds * 1000);
  if (step >= 86400) {
    return new Intl.DateTimeFormat(locale(), { day: "numeric", month: "numeric" }).format(date);
  }
  return new Intl.DateTimeFormat(locale(), {
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  }).format(date);
}

export function formatNumber(value: number, digits = 0): string {
  return new Intl.NumberFormat(locale(), { maximumFractionDigits: digits }).format(value);
}

export function formatCompact(value: number): string {
  return new Intl.NumberFormat(locale(), {
    notation: "compact",
    maximumFractionDigits: 1,
  }).format(value);
}

/** "v0.4.2" for a release, and just "dev" for a development build. */
export function formatVersion(version: string | null | undefined): string {
  if (!version) {
    return "–";
  }
  return /^\d+\.\d/.test(version) ? `v${version}` : version;
}

/** A clock difference in seconds, "+0.2 s" or "0.0 s". */
export function formatSkew(milliseconds: number): string {
  const seconds = new Intl.NumberFormat(locale(), {
    signDisplay: "exceptZero",
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }).format(milliseconds / 1000);
  return `${seconds} s`;
}

export function percentOf(used: number, total: number): number {
  if (!total) {
    return 0;
  }
  return Math.min(100, Math.max(0, (used / total) * 100));
}
