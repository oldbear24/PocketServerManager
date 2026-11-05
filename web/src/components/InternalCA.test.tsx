import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AuthProvider } from "../lib/auth";
import { caApi } from "../lib/certificates";
import { pb, type User } from "../lib/pb";
import { guessPlatform, InternalCACard } from "./InternalCA";

function signInAs(role: User["role"]) {
  const payload = btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600, type: "auth" }));
  pb.authStore.save(`header.${payload}.signature`, {
    id: "u1",
    collectionId: "users",
    collectionName: "users",
    email: `${role}@example.com`,
    name: "",
    role,
  } as unknown as User);
}

function renderCard() {
  return render(
    <MemoryRouter>
      <AuthProvider>
        <InternalCACard />
      </AuthProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
  pb.authStore.clear();
});

describe("the internal CA", () => {
  it("says what the root signs, offers it for download and how to install it", async () => {
    vi.spyOn(caApi, "info").mockResolvedValue({
      root: {
        subject: "PocketCloud Root CA",
        not_after: "2046-11-05T12:00:00Z",
        sha256: "AB:CD:EF",
      },
      agents: 7,
      lan_certificates: 3,
    });
    signInAs("admin");
    renderCard();

    expect(
      await screen.findByText(
        /PocketCloud Root CA · valid until .*2046.*agents \(7\) and of LAN names \(3\)/,
      ),
    ).toBeInTheDocument();
    const download = screen.getByRole("link", { name: "Download the root certificate" });
    expect(download.getAttribute("href")).toMatch(/\/api\/pc\/ca\.crt$/);
    expect(download).toHaveAttribute("download", "pocketcloud-root-ca.crt");
    expect(
      screen
        .getByRole("link", { name: "Download the root certificate as PEM" })
        .getAttribute("href"),
    ).toMatch(/\/api\/pc\/ca\.pem$/);
    expect(screen.getByRole("link", { name: "Agent certificates" })).toHaveAttribute(
      "href",
      "/agents",
    );

    fireEvent.click(screen.getByText("How to install it"));
    expect(screen.getByText("AB:CD:EF")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Device"), { target: { value: "ios" } });
    const steps = screen.getByRole("list");
    expect(within(steps).getAllByRole("listitem")).toHaveLength(3);
    expect(within(steps).getByText(/Certificate Trust Settings/)).toBeInTheDocument();
    // Windows: the command needs an administrator
    fireEvent.change(screen.getByLabelText("Device"), { target: { value: "windows" } });
    expect(screen.getByText("Or in PowerShell, run as administrator:")).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Device"), { target: { value: "ios" } });
    // a phone has no terminal
    expect(screen.queryByRole("region", { name: "Command" })).toBeNull();
    fireEvent.change(screen.getByLabelText("Device"), { target: { value: "linux" } });
    expect(screen.getByRole("region", { name: "Command" }).textContent).toContain(
      "update-ca-certificates",
    );
  });

  it("shows the agents' certificates to admins only", async () => {
    vi.spyOn(caApi, "info").mockResolvedValue({
      root: { subject: "PocketCloud Root CA", not_after: "2046-11-05T12:00:00Z", sha256: "AB" },
      agents: 1,
      lan_certificates: 0,
    });
    signInAs("viewer");
    renderCard();
    expect(await screen.findByText(/PocketCloud Root CA/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Agent certificates" })).toBeNull();
    expect(screen.getByRole("link", { name: "Download the root certificate" })).toBeInTheDocument();
  });

  it("says so when the CA is not open", async () => {
    vi.spyOn(caApi, "info").mockRejectedValue(new Error("The internal CA is not open yet."));
    renderCard();
    expect(await screen.findByText("The internal CA is not open yet.")).toBeInTheDocument();
  });

  it("shows the steps of the device it is opened on first", () => {
    expect(guessPlatform("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)")).toBe("ios");
    expect(guessPlatform("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Firefox/140.0")).toBe(
      "firefox",
    );
    expect(guessPlatform("Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) Safari/605")).toBe("macos");
    expect(guessPlatform("Mozilla/5.0 (Linux; Android 15) Chrome/140")).toBe("android");
    // Firefox on a phone uses the phone's certificates
    expect(
      guessPlatform("Mozilla/5.0 (Android 14; Mobile; rv:132.0) Gecko/132.0 Firefox/132.0"),
    ).toBe("android");
    // an iPad says it is a Mac
    expect(guessPlatform("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Safari/605", 5)).toBe(
      "ios",
    );
    expect(guessPlatform("Mozilla/5.0 (X11; Linux x86_64) Chrome/140")).toBe("linux");
  });
});
