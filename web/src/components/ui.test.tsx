import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { ComponentDemoPage } from "../pages/ComponentDemo";
import {
  Button,
  CodeBlock,
  ConfirmDialog,
  IssueBanner,
  Kpi,
  ProgressBar,
  StatusPill,
  Tabs,
} from "./ui";

describe("the component demo page", () => {
  it("has no accessibility violations", async () => {
    const { container } = render(
      <MemoryRouter>
        <main>
          <ComponentDemoPage />
        </main>
      </MemoryRouter>,
    );

    const results = await axe.run(container, {
      // jsdom has no layout, so colours cannot be measured here
      rules: { "color-contrast": { enabled: false } },
    });

    expect(results.violations.map((violation) => `${violation.id}: ${violation.help}`)).toEqual([]);
  });
});

describe("Tabs", () => {
  function Example() {
    const [active, setActive] = useState("a");
    return (
      <Tabs
        label="Example"
        active={active}
        onChange={setActive}
        items={[
          { id: "a", label: "First" },
          { id: "b", label: "Second" },
          { id: "c", label: "Third" },
        ]}
      />
    );
  }

  it("marks the active tab and switches on click", async () => {
    render(<Example />);

    expect(screen.getByRole("tab", { name: "First" })).toHaveAttribute("aria-selected", "true");

    await userEvent.click(screen.getByRole("tab", { name: "Second" }));

    expect(screen.getByRole("tab", { name: "Second" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "First" })).toHaveAttribute("aria-selected", "false");
  });

  it("moves with the arrow keys and wraps around", async () => {
    render(<Example />);

    screen.getByRole("tab", { name: "First" }).focus();

    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "Second" })).toHaveFocus();

    await userEvent.keyboard("{ArrowLeft}{ArrowLeft}");
    expect(screen.getByRole("tab", { name: "Third" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "Third" })).toHaveAttribute("aria-selected", "true");
  });
});

describe("CodeBlock", () => {
  it("copies to the clipboard and confirms it", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();

    render(<CodeBlock label="Command" code="docker run nginx" />);

    await user.click(screen.getByRole("button", { name: "Copy" }));

    expect(writeText).toHaveBeenCalledWith("docker run nginx");
    expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
  });
});

describe("StatusPill", () => {
  it("always shows its status as text", () => {
    render(<StatusPill tone="err">Offline</StatusPill>);
    expect(screen.getByText("Offline")).toBeInTheDocument();
  });
});

describe("Kpi", () => {
  it("shows a status figure as its word with an icon, in its tone", () => {
    render(
      <Kpi
        label="Integrity check"
        value="OK"
        tone="ok"
        icon={<svg data-testid="icon" aria-hidden="true" />}
        note="restic check · Sunday 06:00"
      />,
    );
    const value = screen.getByText("OK");
    expect(value).toHaveClass("kpi-value", "ok", "with-icon");
    expect(value).toContainElement(screen.getByTestId("icon"));
    expect(screen.getByText("restic check · Sunday 06:00")).toHaveClass("kpi-note");
  });

  it("is a plain figure without a tone", () => {
    render(<Kpi label="Stored" value="1.8 TB" />);
    expect(screen.getByText("1.8 TB").className).toBe("kpi-value");
  });
});

describe("IssueBanner", () => {
  it("is a section named by its title, never an alert that reads out again", () => {
    render(
      <IssueBanner
        id="run-r1"
        tone="err"
        icon={<svg aria-hidden="true" />}
        title="photos @ nas failed at 03:00"
        detail="repository is already locked by PID 4121 on nas (02:58)"
        meta="attempt 1 of 4"
        actions={<Button variant="critical">Unlock and retry</Button>}
      />,
    );
    const banner = screen.getByRole("region", { name: "photos @ nas failed at 03:00" });
    expect(banner).toHaveClass("issue-banner", "err");
    expect(banner).not.toHaveAttribute("role");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "photos @ nas failed at 03:00" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/already locked/)).toHaveClass("issue-banner-detail", "mono");
    expect(screen.getByText("attempt 1 of 4")).toHaveClass("issue-banner-meta");
    expect(screen.getByRole("button", { name: "Unlock and retry" })).toHaveClass(
      "button",
      "critical",
    );
  });

  it("leaves out what it is not given", () => {
    const { container } = render(
      <IssueBanner id="x" tone="info" icon={null} title="Trying again…" />,
    );
    expect(container.querySelector(".issue-banner-detail")).toBeNull();
    expect(container.querySelector(".issue-banner-meta")).toBeNull();
    expect(container.querySelector(".issue-banner-actions")).toBeNull();
  });
});

describe("ProgressBar", () => {
  it("exposes its value and label", () => {
    render(<ProgressBar value={42.4} label="CPU of nas" />);

    const bar = screen.getByRole("progressbar", { name: "CPU of nas" });
    expect(bar).toHaveAttribute("aria-valuenow", "42");
  });

  it.each([
    [10, "progress"],
    [85, "progress warn"],
    [95, "progress err"],
  ])("colours a value of %i", (value, className) => {
    render(<ProgressBar value={value} label="x" />);
    expect(screen.getByRole("progressbar")).toHaveClass(...className.split(" "));
  });
});

describe("ConfirmDialog", () => {
  it("calls back on both buttons", async () => {
    const confirm = vi.fn();
    const cancel = vi.fn();

    render(
      <ConfirmDialog
        open
        title="Sure?"
        confirmLabel="Remove"
        onConfirm={confirm}
        onCancel={cancel}
      />,
    );

    // jsdom has no showModal, and an unopened dialog is hidden from roles
    await userEvent.click(screen.getByRole("button", { name: "Remove", hidden: true }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel", hidden: true }));

    expect(confirm).toHaveBeenCalledOnce();
    expect(cancel).toHaveBeenCalledOnce();
  });
});
