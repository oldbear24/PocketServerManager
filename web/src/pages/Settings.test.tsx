import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { api } from "../lib/api";
import type { PanelInfo } from "../lib/types";
import { AboutCard } from "./Settings";

afterEach(() => {
  vi.restoreAllMocks();
});

const info = {
  version: "v0.1.0",
  public_url: "https://panel.example.com",
  grpc_address: "panel.example.com:9443",
  gateway_running: true,
  ca_pin: "sha256:ab",
  agent_image: "",
  updater_image: "",
  images_pinned: true,
  agents: { connected: 0, total: 0 },
} as PanelInfo;

function about(fields: Partial<PanelInfo>) {
  vi.spyOn(api, "info").mockResolvedValue({ ...info, ...fields });
  return render(
    <MemoryRouter>
      <AboutCard />
    </MemoryRouter>,
  );
}

describe("the panel's HTTPS in Settings (#63)", () => {
  it("links to the certificate the panel serves", async () => {
    about({ https: true, panel_certificate: "p1" });
    const link = await screen.findByRole("link", { name: "The panel's own certificate" });
    expect(link.getAttribute("href")).toBe("/domains?certificate=p1");
  });

  it("says when it is still ordered", async () => {
    about({ https: true, panel_certificate: "" });
    expect(
      await screen.findByText("Served by the panel; its certificate is being ordered"),
    ).toBeTruthy();
  });

  it("claims no plain HTTP without --https-addr, which PocketBase's own HTTPS may serve", async () => {
    about({ https: false });
    expect(
      await screen.findByText("Not managed by the panel (--https-addr is not set)"),
    ).toBeTruthy();
    expect(screen.queryByText(/plain HTTP/)).toBeNull();
  });
});
