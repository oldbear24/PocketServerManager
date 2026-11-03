import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { ComponentDemoPage } from "../pages/ComponentDemo";
import { CodeBlock, ConfirmDialog, ProgressBar, StatusPill, Tabs } from "./ui";

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
