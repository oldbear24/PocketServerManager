import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RequireAuth, RequireRole } from "./App";
import { AuthProvider, hasRole } from "./lib/auth";
import { pb, type User } from "./lib/pb";

function signInAs(role: User["role"]) {
  // a token that is valid for the SDK: only its expiry is looked at on the client
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

afterEach(() => {
  pb.authStore.clear();
  vi.restoreAllMocks();
});

function renderAt(path: string, ui: React.ReactNode) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <Routes>
          <Route path="/login" element={<div>the sign-in page</div>} />
          <Route path="*" element={ui} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
}

describe("protected routes", () => {
  it("redirect to the sign-in page without a session", () => {
    renderAt(
      "/servers/abc",
      <RequireAuth>
        <div>a protected page</div>
      </RequireAuth>,
    );

    expect(screen.getByText("the sign-in page")).toBeInTheDocument();
    expect(screen.queryByText("a protected page")).not.toBeInTheDocument();
  });

  it("show the page with a session", () => {
    signInAs("viewer");

    renderAt(
      "/servers/abc",
      <RequireAuth>
        <div>a protected page</div>
      </RequireAuth>,
    );

    expect(screen.getByText("a protected page")).toBeInTheDocument();
  });
});

describe("role-limited pages", () => {
  const diagnostics = (
    <RequireRole role="admin">
      <div>diagnostics</div>
    </RequireRole>
  );

  it.each(["viewer", "operator"] as const)("keep a %s out of an admin page", (role) => {
    signInAs(role);
    renderAt("/diagnostics", diagnostics);

    expect(screen.queryByText("diagnostics")).not.toBeInTheDocument();
    expect(screen.getByText("You have no access to this page")).toBeInTheDocument();
  });

  it("let an admin in", () => {
    signInAs("admin");
    renderAt("/diagnostics", diagnostics);

    expect(screen.getByText("diagnostics")).toBeInTheDocument();
  });
});

describe("hasRole", () => {
  const user = (role: User["role"]) => ({ role }) as User;

  it("knows the order of the roles", () => {
    expect(hasRole(user("admin"), "operator")).toBe(true);
    expect(hasRole(user("operator"), "operator")).toBe(true);
    expect(hasRole(user("viewer"), "operator")).toBe(false);
    expect(hasRole(null, "viewer")).toBe(false);
  });
});
