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
};

export const phpApi = {
  versions: () => pb.send<PhpVersions>("/api/pc/php/versions", {}),
};

/** Whether a version is to be warned about: it ends soon or has ended. */
export function phpNeedsWarning(version: PhpVersion): boolean {
  return version.state === "ending" || version.state === "ended";
}
