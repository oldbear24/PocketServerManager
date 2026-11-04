import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import { phpApi, type PhpVersion } from "../lib/php";
import { PhpEolWarning, PhpVersionsCard } from "./PhpVersions";
import { ToastProvider } from "./toast";

function version(overrides: Partial<PhpVersion>): PhpVersion {
  return {
    version: "8.4",
    image: "ghcr.io/oldbear24/pocketcloud-php:8.4-fpm",
    released: "2024-11-21",
    active_support_until: "2026-12-31",
    security_support_until: "2028-12-31",
    state: "supported",
    days_left: 788,
    ...overrides,
  };
}

const php82 = version({
  version: "8.2",
  image: "ghcr.io/oldbear24/pocketcloud-php:8.2-fpm",
  released: "2022-12-08",
  active_support_until: "2024-12-31",
  security_support_until: "2026-12-31",
  state: "ending",
  days_left: 57,
});

afterEach(async () => {
  vi.restoreAllMocks();
  await i18n.changeLanguage("en");
});

describe("PHP versions", () => {
  it("warns about PHP 8.2 before its security support ends", async () => {
    vi.spyOn(phpApi, "versions").mockResolvedValue({
      warn_days: 90,
      versions: [
        version({ version: "8.5", image: "ghcr.io/oldbear24/pocketcloud-php:8.5-fpm" }),
        version({}),
        php82,
      ],
    });

    render(
      <ToastProvider>
        <PhpVersionsCard />
      </ToastProvider>,
    );

    expect(
      await screen.findByText(/PHP 8\.2 gets security fixes for 57 more days, until Dec 31, 2026/),
    ).toBeInTheDocument();
    expect(screen.getByText("Ending soon")).toBeInTheDocument();
    expect(screen.getAllByText("Supported")).toHaveLength(2);
    expect(screen.getByText("ghcr.io/oldbear24/pocketcloud-php:8.2-fpm")).toBeInTheDocument();
    // the versions that are fine have no warning
    expect(screen.queryByText(/PHP 8\.4 gets/)).not.toBeInTheDocument();
  });

  it("says when a version has ended", () => {
    render(<PhpEolWarning version={{ ...php82, state: "ended", days_left: -1 }} />);
    expect(
      screen.getByText(/PHP 8\.2 has had no security fixes since Dec 31, 2026/),
    ).toBeInTheDocument();
  });

  it("shows nothing for a supported version", () => {
    const { container } = render(<PhpEolWarning version={version({})} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("counts the days in Czech", async () => {
    await i18n.changeLanguage("cs");
    render(<PhpEolWarning version={{ ...php82, days_left: 3 }} />);
    expect(screen.getByText(/ještě 3 dny/)).toBeInTheDocument();
  });
});
