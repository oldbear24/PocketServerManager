import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ToastProvider } from "../components/toast";
import { api } from "../lib/api";
import { AuthProvider } from "../lib/auth";
import { pb, type User } from "../lib/pb";
import type { Container, Host } from "../lib/types";
import { ContainersCard } from "./ServerDetail";

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

function container(name: string, state: string, extra: Partial<Container> = {}): Container {
  return {
    id: name,
    collectionId: "containers",
    collectionName: "containers",
    host: "h1",
    stack: "",
    project: "",
    service: "",
    docker_id: `id-${name}`,
    name,
    image: "nginx:1",
    state,
    status_text: "",
    health: "",
    system_role: "",
    restart_count: 0,
    ports: null,
    labels: null,
    started_at: "",
    ...extra,
  } as Container;
}

const containers = [
  container("web", "running"),
  container("worker", "exited"),
  container("pocketcloud-agent", "running", { system_role: "agent", image: "agent:1" }),
];

function renderCard(role: User["role"]) {
  signInAs(role);
  vi.spyOn(pb, "send").mockResolvedValue({});
  const run = vi.spyOn(api, "runCommand").mockResolvedValue({} as never);
  render(
    <MemoryRouter>
      <AuthProvider>
        <ToastProvider>
          <ContainersCard containers={containers} host={{ id: "h1" } as Host} />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
  return run;
}

afterEach(() => {
  pb.authStore.clear();
  vi.restoreAllMocks();
});

describe("the containers table", () => {
  it("lets operators choose containers, but not the system ones", () => {
    renderCard("operator");

    expect(screen.getByRole("checkbox", { name: "Choose web" })).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Choose worker" })).toBeInTheDocument();
    expect(
      screen.queryByRole("checkbox", { name: "Choose pocketcloud-agent" }),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("checkbox", { name: "Choose all containers" }));
    const bar = screen.getByRole("toolbar", { name: "Actions for the chosen containers" });
    expect(within(bar).getByText("2 chosen")).toBeInTheDocument();
  });

  it("acts on the chosen containers that can take the action", () => {
    const run = renderCard("operator");

    fireEvent.click(screen.getByRole("checkbox", { name: "Choose all containers" }));
    const bar = screen.getByRole("toolbar");

    fireEvent.click(within(bar).getByRole("button", { name: "Stop" }));
    expect(run).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenCalledWith("h1", "container.stop", { docker_id: "id-web" }, "web");

    // both run nginx:1, which is pulled once
    run.mockClear();
    fireEvent.click(within(bar).getByRole("button", { name: "Pull image" }));
    expect(run).toHaveBeenCalledTimes(1);
    expect(run).toHaveBeenCalledWith("h1", "image.pull", { image: "nginx:1" }, "nginx:1");
  });

  it("asks before removing the chosen containers, without their volumes by default", () => {
    const run = renderCard("operator");

    fireEvent.click(screen.getByRole("checkbox", { name: "Choose all containers" }));
    fireEvent.click(within(screen.getByRole("toolbar")).getByRole("button", { name: "Remove" }));
    expect(run).not.toHaveBeenCalled();

    // jsdom does not open a modal dialog, so its content counts as hidden
    const dialog = screen.getByRole("dialog", { hidden: true });
    expect(within(dialog).getByText("Remove 2 containers?")).toBeInTheDocument();
    expect(within(dialog).getByRole("checkbox", { hidden: true })).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove", hidden: true }));

    expect(run).toHaveBeenCalledTimes(2);
    expect(run).toHaveBeenCalledWith(
      "h1",
      "container.remove",
      { docker_id: "id-worker", remove_volumes: false, force: true },
      "worker",
    );
  });

  it("shows viewers no choice and no actions", () => {
    renderCard("viewer");

    expect(screen.queryAllByRole("checkbox")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: /Stop|Remove|Restart/ })).not.toBeInTheDocument();
  });
});
