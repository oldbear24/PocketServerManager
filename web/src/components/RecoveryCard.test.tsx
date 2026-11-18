import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ClientResponseError } from "pocketbase";
import { MemoryRouter } from "react-router-dom";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import i18n from "../i18n";
import { recoveryApi, type KitCheck, type RecoveryState } from "../lib/recovery";
import { RecoveryCard } from "./RecoveryCard";
import { ToastProvider } from "./toast";

// Tuesday 17 November 2026, 10:00 UTC
const now = Date.parse("2026-11-17T10:00:00Z");

vi.mock("../lib/realtime", () => ({ useNow: () => now }));

beforeAll(() => {
  HTMLDialogElement.prototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
});

afterEach(async () => {
  // unmounted first, so that the language going back renders nothing outside act
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  await i18n.changeLanguage("en");
});

afterAll(async () => {
  await i18n.changeLanguage("en");
});

const pin = `sha256:${"5d1e".repeat(16)}`;

function state(fields: Partial<RecoveryState> = {}): RecoveryState {
  return {
    key_id: "9f2c41d0a1b2c3d4e5f60718293a4b5c",
    ca_pin: pin,
    kit: { status: "never", reasons: null },
    addresses: [{ address: "panel.example.com:9443", servers: 3, ip: false }],
    unreported: 0,
    start_env: {
      POCKETCLOUD_PUBLIC_URL: "https://panel.example.com",
      POCKETCLOUD_GRPC_PUBLIC_ADDR: "",
      POCKETCLOUD_EXPECT_CA_PIN: pin,
    },
    panel_job: {
      id: "jp",
      name: "panel (pb_data)",
      enabled: true,
      carrier: "h1",
      carrier_name: "vm",
      targets: ["panel-sftp", "panel-minio"],
      last_success_at: "2026-11-17 04:00:40.000Z",
    },
    restored: null,
    ...fields,
  };
}

function failure(status: number, message: string) {
  return new ClientResponseError({ status, response: { message }, url: "", isAbort: false });
}

function show(loaded: RecoveryState | Error, path = "/settings") {
  const get = vi.spyOn(recoveryApi, "getRecovery");
  if (loaded instanceof Error) {
    get.mockRejectedValue(loaded);
  } else {
    get.mockResolvedValue(loaded);
  }
  render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <RecoveryCard />
      </ToastProvider>
    </MemoryRouter>,
  );
  return get;
}

function card() {
  return within(document.getElementById("recovery-kit")!);
}

function dialog(name: string) {
  return within(screen.getByRole("dialog", { name }));
}

/** The dialog's own confirm button, which the card's buttons may share a name with. */
function confirm(name: string, label: string) {
  return dialog(name).getByRole("button", { name: label });
}

const kitText = `This file restores a PocketCloud panel: docs/disaster-recovery.md. It is sealed with a passphrase.
-----BEGIN POCKETCLOUD RECOVERY KIT-----
Kit: KIT-7Q2M-9XHC
-----END POCKETCLOUD RECOVERY KIT-----
`;

describe("the recovery kit's state", () => {
  it("says a kit was never downloaded, and what to do", async () => {
    show(state());
    expect(await card().findByText("Never downloaded")).toHaveClass("pill", "warn");
    expect(card().getByRole("heading", { name: "Recovery kit" })).toBeInTheDocument();
    expect(
      card().getByText("Download the kit, keep it off this machine, then check it here."),
    ).toBeInTheDocument();
    expect(card().getByText("9f2c41d0a1b2c3d4e5f60718293a4b5c")).toBeInTheDocument();
    expect(card().getByText(pin)).toBeInTheDocument();
    expect(card().getByRole("button", { name: "Download a recovery kit…" })).toHaveClass("primary");
    expect(card().getByRole("button", { name: "Check a saved kit…" })).toBeInTheDocument();
    expect(card().getByText("Recovery guide: docs/disaster-recovery.md")).toBeInTheDocument();
  });

  it("says a downloaded kit is not checked yet", async () => {
    show(
      state({ kit: { status: "unverified", reasons: null, created: "2026-11-17 09:55:00.000Z" } }),
    );
    expect(await card().findByText("Downloaded 5 min. ago, not checked yet")).toHaveClass("warn");
    expect(
      card().getByText(
        "Check the kit you saved: until a check opens it, PocketCloud keeps reminding you.",
      ),
    ).toBeInTheDocument();
  });

  it("names what made a kit stale, in both languages", async () => {
    show(state({ kit: { status: "stale", reasons: ["address", "targets"] } }));
    expect(
      await card().findByText(
        "Out of date: the panel's addresses changed, a target of the panel's backup changed",
      ),
    ).toHaveClass("warn");
    expect(
      card().getByText(
        "Download a new kit and check it: the old one would not restore the panel as it is now.",
      ),
    ).toBeInTheDocument();
    await act(() => i18n.changeLanguage("cs"));
    expect(
      card().getByText("Zastaralá: změnily se adresy panelu, změnil se cíl zálohy panelu"),
    ).toBeInTheDocument();
  });

  it("shows a saved kit as OK, with who checked it, and no reminder", async () => {
    show(
      state({
        kit: {
          status: "saved",
          reasons: null,
          kit_id: "KIT-7Q2M-9XHC",
          verified_at: "2026-11-16 10:00:00.000Z",
          verified_by: "admin@example.com",
        },
      }),
    );
    expect(await card().findByText("Saved: checked yesterday by admin@example.com")).toHaveClass(
      "pill",
      "ok",
    );
    expect(card().queryByRole("status")).not.toBeInTheDocument();
    expect(card().getByRole("button", { name: "Download a recovery kit…" })).not.toHaveClass(
      "primary",
    );
  });

  it("says it is loading, and offers nothing yet", () => {
    vi.spyOn(recoveryApi, "getRecovery").mockReturnValue(new Promise(() => {}));
    render(
      <MemoryRouter>
        <ToastProvider>
          <RecoveryCard />
        </ToastProvider>
      </MemoryRouter>,
    );
    expect(card().getByText("Loading…")).toBeInTheDocument();
    expect(card().queryByRole("button")).not.toBeInTheDocument();
  });

  it("shows the panel's sentence when the state cannot be read", async () => {
    show(failure(403, "Only admins can do this."));
    expect(await card().findByRole("alert")).toHaveTextContent("Only admins can do this.");
    expect(card().queryByText("Loading…")).not.toBeInTheDocument();
  });

  it("scrolls itself into view from a link to it", async () => {
    const scroll = vi.fn();
    HTMLElement.prototype.scrollIntoView = scroll;
    show(state(), "/settings#recovery-kit");
    await card().findByText("Never downloaded");
    expect(scroll).toHaveBeenCalledWith({ block: "start" });
  });
});

describe("the panel's backup job on the card", () => {
  it("links to setting it up when there is none", async () => {
    show(state({ panel_job: null }));
    expect(
      await card().findByText("Not set up: the kit holds the master key only."),
    ).toBeInTheDocument();
    expect(card().getByRole("link", { name: "Back up the panel" })).toHaveAttribute(
      "href",
      "/backups?setup=panel",
    );
  });

  it("says when it last succeeded, through which server", async () => {
    show(state());
    expect(await card().findByText("Last succeeded 6 hr. ago · via vm")).toBeInTheDocument();
    expect(card().getByRole("link", { name: "panel (pb_data)" })).toHaveAttribute(
      "href",
      "/backups?job=jp",
    );
    expect(card().getByText(/→ panel-sftp \+ panel-minio/)).toBeInTheDocument();
  });

  it("says when it never succeeded, and when it is paused", async () => {
    show(state({ panel_job: { ...state().panel_job!, last_success_at: "", enabled: false } }));
    expect(await card().findByText("Not succeeded yet")).toBeInTheDocument();
    expect(card().getByText("· paused")).toBeInTheDocument();
  });
});

describe("the addresses the servers dial", () => {
  it("warns of each IP address, and counts the servers in both languages", async () => {
    show(
      state({
        addresses: [
          { address: "192.0.2.2:9443", servers: 1, ip: true },
          { address: "panel.example.com:9443", servers: 2, ip: false },
          { address: "[2001:db8::2]:9443", servers: 5, ip: true },
        ],
        unreported: 1,
      }),
    );
    expect(await card().findByText("192.0.2.2:9443 · 1 server")).toBeInTheDocument();
    expect(card().getByText("panel.example.com:9443 · 2 servers")).toBeInTheDocument();
    const warnings = card()
      .getAllByRole("status")
      .map((banner) => banner.textContent);
    expect(warnings).toEqual([
      "Download the kit, keep it off this machine, then check it here.",
      "192.0.2.2:9443 is an IP address: a new panel machine must take it over, or each of its servers needs pocketcloud-agent repoint.",
      "[2001:db8::2]:9443 is an IP address: a new panel machine must take it over, or each of its servers needs pocketcloud-agent repoint.",
    ]);
    expect(
      card().getByText("1 server has not said yet which address it dials."),
    ).toBeInTheDocument();

    await act(() => i18n.changeLanguage("cs"));
    expect(card().getByText("192.0.2.2:9443 · 1 server")).toBeInTheDocument();
    expect(card().getByText("panel.example.com:9443 · 2 servery")).toBeInTheDocument();
    expect(card().getByText("[2001:db8::2]:9443 · 5 serverů")).toBeInTheDocument();
    expect(
      card().getByText("1 server zatím neřekl, na jakou adresu se připojuje."),
    ).toBeInTheDocument();
  });

  it("counts the servers that have not said yet", async () => {
    show(state({ addresses: [], unreported: 4 }));
    expect(
      await card().findByText("4 servers have not said yet which address they dial."),
    ).toBeInTheDocument();
    await act(() => i18n.changeLanguage("cs"));
    expect(
      card().getByText("4 servery zatím neřekly, na jakou adresu se připojují."),
    ).toBeInTheDocument();
  });

  it("gives the environment a restored panel starts with, leaving out what is empty", async () => {
    show(state());
    const code = await card().findByRole("region", { name: "Start a restored panel with" });
    expect(code.textContent).toBe(
      `POCKETCLOUD_PUBLIC_URL=https://panel.example.com\nPOCKETCLOUD_EXPECT_CA_PIN=${pin}`,
    );
  });

  it("orders the environment as recover's env file does, whatever order the panel's answer has", async () => {
    show(
      state({
        start_env: {
          POCKETCLOUD_EXPECT_CA_PIN: pin,
          POCKETCLOUD_PUBLIC_URL: "https://panel.example.com",
          POCKETCLOUD_HTTPS_CHALLENGE: "dns",
          POCKETCLOUD_GRPC_ADDR: ":9443",
        },
      }),
    );
    const code = await card().findByRole("region", { name: "Start a restored panel with" });
    expect(code.textContent).toBe(
      [
        "POCKETCLOUD_GRPC_ADDR=:9443",
        "POCKETCLOUD_HTTPS_CHALLENGE=dns",
        "POCKETCLOUD_PUBLIC_URL=https://panel.example.com",
        `POCKETCLOUD_EXPECT_CA_PIN=${pin}`,
      ].join("\n"),
    );
  });

  it("shows no addresses without servers", async () => {
    show(state({ addresses: null, unreported: 0 }));
    await card().findByText("Never downloaded");
    expect(card().queryByText("Servers dial the panel at")).not.toBeInTheDocument();
  });
});

describe("a restored panel", () => {
  const restored = {
    at: "2026-11-17 09:58:00.000Z",
    from: "2026-11-17 04:00:00.000Z",
    snapshot: "4f1c2d3e",
    back: 2,
    total: 3,
  };

  it("says when it was restored, from when, and how many servers are back", async () => {
    show(state({ restored }));
    const block = await card().findByRole("region", { name: "Restored from a backup" });
    expect(block).toHaveTextContent(/^Restored from a backupRestored .* from a backup made .*\./);
    expect(within(block).getByText("Servers back: 2 of 3.")).toBeInTheDocument();
    await act(() => i18n.changeLanguage("cs"));
    expect(within(block).getByText("Servery zpět: 2 z 3.")).toBeInTheDocument();
  });

  it("asks again every 10 seconds while servers are coming back", async () => {
    vi.useFakeTimers();
    const get = show(state({ restored }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(get).toHaveBeenCalledTimes(1);
    get.mockResolvedValue(state({ restored: { ...restored, back: 3 } }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(get).toHaveBeenCalledTimes(2);
    expect(screen.getByText("Servers back: 3 of 3.")).toBeInTheDocument();
    // every server is back: no more asking
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(get).toHaveBeenCalledTimes(2);
  });

  it("stays 7 days once every server is back, and goes then", async () => {
    show(state({ restored: { ...restored, back: 3, at: "2026-11-11 10:00:00.000Z" } }));
    expect(
      await card().findByRole("region", { name: "Restored from a backup" }),
    ).toBeInTheDocument();
    vi.restoreAllMocks();
    document.body.innerHTML = "";
    show(state({ restored: { ...restored, back: 3, at: "2026-11-10 09:59:00.000Z" } }));
    await card().findByText("Never downloaded");
    expect(
      card().queryByRole("region", { name: "Restored from a backup" }),
    ).not.toBeInTheDocument();
  });
});

describe("downloading a kit", () => {
  async function openDownload(loaded = state()) {
    const get = show(loaded);
    fireEvent.click(await card().findByRole("button", { name: "Download a recovery kit…" }));
    return get;
  }

  const title = "Download a recovery kit";

  function fill(fields: { passphrase?: string; again?: string; password?: string }) {
    const form = dialog(title);
    if (fields.passphrase !== undefined) {
      fireEvent.change(form.getByLabelText("Passphrase", { selector: "input" }), {
        target: { value: fields.passphrase },
      });
    }
    if (fields.again !== undefined) {
      fireEvent.change(form.getByLabelText("Passphrase again"), {
        target: { value: fields.again },
      });
    }
    if (fields.password !== undefined) {
      fireEvent.change(form.getByLabelText("Your sign-in password"), {
        target: { value: fields.password },
      });
    }
  }

  it("says what the kit holds, and asks for a passphrase twice and the sign-in password", async () => {
    await openDownload();
    const form = dialog(title);
    expect(
      form.getByText(/^The kit holds the master key and the credentials and passwords/),
    ).toBeInTheDocument();
    for (const label of ["Passphrase", "Passphrase again"]) {
      expect(form.getByLabelText(label)).toHaveAttribute("type", "password");
      expect(form.getByLabelText(label)).toHaveAttribute("autocomplete", "new-password");
    }
    expect(form.getByLabelText("Your sign-in password")).toHaveAttribute("type", "password");
    expect(form.queryByText(/this kit holds the master key alone/)).not.toBeInTheDocument();
  });

  it("says the kit holds the key alone without a panel job", async () => {
    await openDownload(state({ panel_job: null }));
    expect(
      dialog(title).getByText(
        "The panel has no backup job yet: this kit holds the master key alone.",
      ),
    ).toBeInTheDocument();
  });

  it("holds back a short passphrase, in both languages", async () => {
    await openDownload();
    fill({ passphrase: "eleven char", again: "eleven char", password: "hunter22" });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit and its passphrase apart/));
    expect(dialog(title).getByText("At least 12 characters.")).toBeInTheDocument();
    expect(dialog(title).getByLabelText("Passphrase", { selector: "input" })).toHaveAttribute(
      "aria-invalid",
      "true",
    );
    expect(confirm(title, "Download")).toBeDisabled();
    await act(() => i18n.changeLanguage("cs"));
    expect(screen.getByRole("dialog", { name: "Stažení záchranné sady" })).toHaveTextContent(
      "Aspoň 12 znaků.",
    );
    await act(() => i18n.changeLanguage("en"));
    fill({ passphrase: "twelve chars", again: "twelve chars" });
    expect(dialog(title).queryByText("At least 12 characters.")).not.toBeInTheDocument();
    expect(confirm(title, "Download")).toBeEnabled();
  });

  it("counts a passphrase's characters in NFC", async () => {
    await openDownload();
    // 12 characters, 17 code units once decomposed
    const passphrase = "kůň žluťouč!".normalize("NFD");
    expect(passphrase.length).toBeGreaterThan(12);
    fill({ passphrase, again: passphrase, password: "hunter22" });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    expect(dialog(title).queryByText("At least 12 characters.")).not.toBeInTheDocument();
    expect(confirm(title, "Download")).toBeEnabled();
    fill({ passphrase: "kůň žluťouč".normalize("NFD"), again: "kůň žluťouč".normalize("NFD") });
    expect(dialog(title).getByText("At least 12 characters.")).toBeInTheDocument();
    expect(confirm(title, "Download")).toBeDisabled();
  });

  it("holds back passphrases that differ", async () => {
    await openDownload();
    fill({ passphrase: "correct horse battery", again: "correct horse batterY", password: "x" });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    expect(dialog(title).getByText("The passphrases differ.")).toBeInTheDocument();
    expect(dialog(title).getByLabelText("Passphrase again")).toHaveAttribute(
      "aria-invalid",
      "true",
    );
    expect(confirm(title, "Download")).toBeDisabled();
    fill({ again: "correct horse battery" });
    expect(dialog(title).queryByText("The passphrases differ.")).not.toBeInTheDocument();
    expect(confirm(title, "Download")).toBeEnabled();
  });

  it("needs the sign-in password", async () => {
    await openDownload();
    fill({ passphrase: "correct horse battery", again: "correct horse battery" });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    expect(confirm(title, "Download")).toBeDisabled();
    fill({ password: "hunter22" });
    expect(confirm(title, "Download")).toBeEnabled();
  });

  it("needs the kit and its passphrase kept apart", async () => {
    await openDownload();
    fill({
      passphrase: "correct horse battery",
      again: "correct horse battery",
      password: "hunter22",
    });
    const apart = dialog(title).getByLabelText(
      "I keep the kit and its passphrase apart, off this machine and away from the backups.",
    );
    expect(apart).not.toBeChecked();
    expect(apart).toBeRequired();
    expect(confirm(title, "Download")).toBeDisabled();
    fireEvent.click(apart);
    expect(confirm(title, "Download")).toBeEnabled();
    fireEvent.click(apart);
    expect(confirm(title, "Download")).toBeDisabled();
  });

  it("generates a passphrase of 125 bits and shows it once", async () => {
    vi.spyOn(crypto, "getRandomValues").mockImplementation((array) => {
      (array as Uint8Array).set(Array.from({ length: 25 }, (_, index) => index));
      return array;
    });
    await openDownload();
    fireEvent.click(dialog(title).getByRole("button", { name: "Generate one" }));
    const code = dialog(title).getByRole("region", { name: "Passphrase" });
    expect(code).toHaveTextContent("01234-56789-ABCDE-FGHJK-MNPQR");
    expect(dialog(title).getByLabelText("Passphrase", { selector: "input" })).toHaveValue(
      "01234-56789-ABCDE-FGHJK-MNPQR",
    );
    expect(dialog(title).getByLabelText("Passphrase again")).toHaveValue(
      "01234-56789-ABCDE-FGHJK-MNPQR",
    );
    expect(
      dialog(title).getByText(
        "Write this passphrase down now: it is kept nowhere and never shown again.",
      ),
    ).toBeInTheDocument();
    // another passphrase typed: the code shown is no longer the one used
    fill({ passphrase: "correct horse battery" });
    expect(dialog(title).queryByRole("region", { name: "Passphrase" })).not.toBeInTheDocument();
  });

  it("clears the passphrases and the password when it closes", async () => {
    await openDownload();
    fireEvent.click(dialog(title).getByRole("button", { name: "Generate one" }));
    fill({ password: "hunter22" });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    fireEvent.click(dialog(title).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog", { name: title })).not.toBeInTheDocument();

    fireEvent.click(card().getByRole("button", { name: "Download a recovery kit…" }));
    const form = dialog(title);
    expect(form.getByLabelText("Passphrase", { selector: "input" })).toHaveValue("");
    expect(form.getByLabelText("Passphrase again")).toHaveValue("");
    expect(form.getByLabelText("Your sign-in password")).toHaveValue("");
    expect(form.getByLabelText(/^I keep the kit/)).not.toBeChecked();
    expect(form.queryByRole("region", { name: "Passphrase" })).not.toBeInTheDocument();
  });

  it("saves the kit as its file, then asks to check it", async () => {
    const get = await openDownload();
    const exportKit = vi.spyOn(recoveryApi, "exportKit").mockResolvedValue({
      kit_id: "KIT-7Q2M-9XHC",
      file_name: "pocketcloud-recovery-kit-20261117-KIT-7Q2M-9XHC.pckit",
      content: kitText,
    });
    const save = vi.spyOn(recoveryApi, "saveTextFile").mockImplementation(() => {});
    fill({
      passphrase: "correct horse battery",
      again: "correct horse battery",
      password: "hunter22",
    });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    fireEvent.click(confirm(title, "Download"));

    expect(exportKit).toHaveBeenCalledWith("correct horse battery", "hunter22");
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith(
        "pocketcloud-recovery-kit-20261117-KIT-7Q2M-9XHC.pckit",
        kitText,
      ),
    );
    expect(
      await screen.findByText(
        "The kit KIT-7Q2M-9XHC was downloaded. Check it now: the reminder stays until a check opens it.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("dialog", { name: title })).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Check a saved kit" })).toBeInTheDocument();
    await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  });

  it("shows the panel's refusal and stays open", async () => {
    await openDownload();
    vi.spyOn(recoveryApi, "exportKit").mockRejectedValue(
      failure(400, "Your password is not right."),
    );
    const save = vi.spyOn(recoveryApi, "saveTextFile").mockImplementation(() => {});
    fill({
      passphrase: "correct horse battery",
      again: "correct horse battery",
      password: "wrong",
    });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    fireEvent.click(confirm(title, "Download"));
    expect(await dialog(title).findByRole("alert")).toHaveTextContent(
      "Your password is not right.",
    );
    expect(save).not.toHaveBeenCalled();
    expect(dialog(title).getByLabelText("Passphrase", { selector: "input" })).toHaveValue(
      "correct horse battery",
    );
  });

  it("says it is sealing while the panel is at it", async () => {
    await openDownload();
    vi.spyOn(recoveryApi, "exportKit").mockReturnValue(new Promise(() => {}));
    fill({
      passphrase: "correct horse battery",
      again: "correct horse battery",
      password: "hunter22",
    });
    fireEvent.click(dialog(title).getByLabelText(/^I keep the kit/));
    fireEvent.click(confirm(title, "Download"));
    expect(await dialog(title).findByRole("button", { name: "Sealing the kit…" })).toBeDisabled();
  });
});

describe("checking a kit", () => {
  const title = "Check a saved kit";

  async function openCheck() {
    const get = show(state({ kit: { status: "unverified", reasons: null, created: "" } }));
    fireEvent.click(await card().findByRole("button", { name: "Check a saved kit…" }));
    return get;
  }

  function choose(file: File) {
    fireEvent.change(dialog(title).getByLabelText("Kit file (.pckit)"), {
      target: { files: [file] },
    });
  }

  function typePassphrase(value: string) {
    fireEvent.change(dialog(title).getByLabelText("Passphrase", { selector: "input" }), {
      target: { value },
    });
  }

  const kitFile = () => new File([kitText], "kit.pckit", { type: "text/plain" });

  it("takes a .pckit file and its passphrase, and needs both", async () => {
    await openCheck();
    const input = dialog(title).getByLabelText("Kit file (.pckit)");
    expect(input).toHaveAttribute("type", "file");
    expect(input).toHaveAttribute("accept", ".pckit,text/plain");
    expect(confirm(title, "Check")).toBeDisabled();
    typePassphrase("correct horse battery");
    expect(confirm(title, "Check")).toBeDisabled();
    choose(kitFile());
    await waitFor(() => expect(confirm(title, "Check")).toBeEnabled());
  });

  it("says a current kit is saved, and refreshes the card", async () => {
    const get = await openCheck();
    const check = vi.spyOn(recoveryApi, "checkKit").mockResolvedValue({
      kit_id: "KIT-7Q2M-9XHC",
      current: true,
      reasons: [],
      status: "saved",
    } satisfies KitCheck);
    choose(kitFile());
    typePassphrase("correct horse battery");
    await waitFor(() => expect(confirm(title, "Check")).toBeEnabled());
    fireEvent.click(confirm(title, "Check"));
    expect(check).toHaveBeenCalledWith(kitText, "correct horse battery");
    expect(
      await dialog(title).findByText("The kit KIT-7Q2M-9XHC opens and is current: it is saved."),
    ).toBeInTheDocument();
    await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  });

  it("says what made an opened kit stale", async () => {
    await openCheck();
    vi.spyOn(recoveryApi, "checkKit").mockResolvedValue({
      kit_id: "KIT-7Q2M-9XHC",
      current: false,
      reasons: ["job"],
      status: "stale",
    });
    choose(kitFile());
    typePassphrase("correct horse battery");
    await waitFor(() => expect(confirm(title, "Check")).toBeEnabled());
    fireEvent.click(confirm(title, "Check"));
    expect(
      await dialog(title).findByText(
        "The kit KIT-7Q2M-9XHC opens, but it is out of date: the panel's backup job changed. Download a new one.",
      ),
    ).toBeInTheDocument();
    expect(dialog(title).queryByText(/is current: it is saved/)).not.toBeInTheDocument();
  });

  it("shows the panel's refusal", async () => {
    await openCheck();
    vi.spyOn(recoveryApi, "checkKit").mockRejectedValue(
      failure(409, "This kit holds another master key (id 1f2e3d4c): it is not this panel's."),
    );
    choose(kitFile());
    typePassphrase("correct horse battery");
    await waitFor(() => expect(confirm(title, "Check")).toBeEnabled());
    fireEvent.click(confirm(title, "Check"));
    expect(await dialog(title).findByRole("alert")).toHaveTextContent(
      "This kit holds another master key (id 1f2e3d4c): it is not this panel's.",
    );
  });

  it("sends no file too big to be a kit", async () => {
    await openCheck();
    const check = vi.spyOn(recoveryApi, "checkKit");
    choose(new File(["x".repeat(64 * 1024 + 1)], "backup.pcpb"));
    typePassphrase("correct horse battery");
    expect(
      await dialog(title).findByText(
        "This file is too big to be a recovery kit: a kit is at most 64 KiB.",
      ),
    ).toBeInTheDocument();
    expect(confirm(title, "Check")).toBeDisabled();
    fireEvent.click(confirm(title, "Check"));
    expect(check).not.toHaveBeenCalled();
    // exactly 64 KiB is taken
    choose(new File(["x".repeat(64 * 1024)], "kit.pckit"));
    await waitFor(() => expect(confirm(title, "Check")).toBeEnabled());
  });

  it("clears its passphrase when it closes", async () => {
    await openCheck();
    typePassphrase("correct horse battery");
    fireEvent.click(dialog(title).getByRole("button", { name: "Cancel" }));
    fireEvent.click(card().getByRole("button", { name: "Check a saved kit…" }));
    expect(dialog(title).getByLabelText("Passphrase", { selector: "input" })).toHaveValue("");
  });
});
