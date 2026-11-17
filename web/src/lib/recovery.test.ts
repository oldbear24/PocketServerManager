import { readFileSync } from "node:fs";

import { afterEach, describe, expect, it, vi } from "vitest";

import { pb } from "./pb";
import {
  dialAddress,
  generatePassphrase,
  panelImage,
  passphraseLength,
  recoveryApi,
  recoveryCommands,
  shellWord,
} from "./recovery";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("the commands of a panel's restore", () => {
  it("name the panel's image beside the agent's, by the version", () => {
    expect(panelImage("ghcr.io/oldbear24/pocketcloud-agent@sha256:5f1c", "0.32.0")).toBe(
      "ghcr.io/oldbear24/pocketcloud-panel:0.32.0",
    );
    expect(panelImage("localhost:5000/pocketcloud-agent:dev", "dev")).toBe(
      "localhost:5000/pocketcloud-panel:latest",
    );
    expect(panelImage("pocketcloud-agent", "0.32.0")).toBe("pocketcloud-panel:0.32.0");
    expect(panelImage("", "")).toBe("ghcr.io/oldbear24/pocketcloud-panel:latest");
  });

  it("take the address most servers dial, else the panel's own", () => {
    expect(
      dialAddress(
        [
          { panel_address: "panel.example.com:9443" },
          { panel_address: "192.0.2.2:9443" },
          { panel_address: "192.0.2.2:9443" },
          {},
        ],
        "localhost:9443",
      ),
    ).toBe("192.0.2.2:9443");
    expect(dialAddress([{ panel_address: "" }, {}], "panel.example.com:9443")).toBe(
      "panel.example.com:9443",
    );
  });

  it("quote what a shell would split", () => {
    expect(shellWord("panel-sftp")).toBe("panel-sftp");
    expect(shellWord("ghcr.io/x/pocketcloud-agent@sha256:5f1c")).toBe(
      "ghcr.io/x/pocketcloud-agent@sha256:5f1c",
    );
    expect(shellWord("Backblaze B2")).toBe("'Backblaze B2'");
    expect(shellWord("it's; rm -rf /")).toBe(`'it'\\''s; rm -rf /'`);
    expect(shellWord("$(id)")).toBe("'$(id)'");
    expect(shellWord("")).toBe("''");
  });

  it("publish the port the servers dial, and read the passphrase without an echo", () => {
    const commands = recoveryCommands({
      snapshot: "latest",
      target: "Backblaze B2",
      agentImage: "ghcr.io/oldbear24/pocketcloud-agent:0.32.0",
      panelImage: "ghcr.io/oldbear24/pocketcloud-panel:0.32.0",
      address: "[2001:db8::2]:19443",
    });
    expect(commands.start).toContain("-p 19443:9443");
    expect(commands.fetch).toContain("--target 'Backblaze B2' --snapshot latest");
    expect(commands.fetch.split("\n")[0]).toBe(
      "read -rs POCKETCLOUD_KIT_PASSPHRASE && export POCKETCLOUD_KIT_PASSPHRASE",
    );
    expect(commands.launcher).toContain("-v /var/run/docker.sock:/var/run/docker.sock");
    // the key is written by recover, and only read once the panel runs
    expect(commands.restore).toContain("-v pocketcloud-secrets:/run/secrets ");
    expect(commands.start).not.toMatch(/pocketcloud-secrets:\/run\/secrets(?!:ro)/);
    // an address without a port: the gateway's own
    const fallback = recoveryCommands({
      snapshot: "",
      target: "",
      agentImage: "",
      panelImage: "",
      address: "",
    });
    expect(fallback.start).toContain("-p 9443:9443");
  });

  it("are the guide's, word for word", () => {
    const values = {
      snapshot: "4f1c2d3e",
      target: "panel-sftp",
      agentImage: "ghcr.io/oldbear24/pocketcloud-agent:0.32.0",
      panelImage: "ghcr.io/oldbear24/pocketcloud-panel:0.32.0",
      address: "192.0.2.2:19443",
    };
    // the drawer's lines, each command on one line as the guide writes it
    const drawer = new Set(
      Object.values(recoveryCommands(values)).flatMap((block) =>
        block.replace(/ \\\n\s*/g, " ").split("\n"),
      ),
    );
    // the guide's commands of its restore, with its placeholders filled in
    const guide = readFileSync("../docs/disaster-recovery.md", "utf8");
    const restore = guide.slice(
      guide.indexOf("## Restore on a new machine"),
      guide.indexOf("## Rolling a running panel back"),
    );
    const commands = [...restore.matchAll(/```sh\n([\s\S]*?)```/g)]
      .flatMap((block) => block[1].trim().split("\n"))
      .map((line) =>
        line
          .trim()
          .replaceAll("<target>", values.target)
          .replaceAll("<id|latest>", values.snapshot)
          .replaceAll("<agent image>", values.agentImage)
          .replaceAll("<panel image>", values.panelImage)
          .replaceAll("<dial port>", "19443"),
      );
    expect(new Set(commands)).toEqual(drawer);
  });
});

describe("a generated passphrase", () => {
  it("is 25 characters of Crockford's base32, in groups of five", () => {
    const passphrase = generatePassphrase();
    expect(passphrase).toMatch(/^[0-9A-HJKMNP-TV-Z]{5}(-[0-9A-HJKMNP-TV-Z]{5}){4}$/);
    expect(passphrase.replace(/-/g, "")).toHaveLength(25);
    expect(generatePassphrase()).not.toBe(passphrase);
  });

  it("takes 5 bits of each random byte, so every character is as likely", () => {
    const bytes = Array.from({ length: 25 }, (_, index) => index * 8 + 7);
    const passphrase = generatePassphrase((target) => target.set(bytes));
    // 7, 15, 23, 31, 39 & 31 = 7 …
    expect(passphrase).toBe("7FQZ7-FQZ7F-QZ7FQ-Z7FQZ-7FQZ7");
    const all = generatePassphrase((target) => target.set(bytes.map((_, index) => index + 224)));
    expect(all).toBe("01234-56789-ABCDE-FGHJK-MNPQR");
  });

  it("comes from the browser's random numbers", () => {
    const random = vi.spyOn(crypto, "getRandomValues");
    generatePassphrase();
    expect(random).toHaveBeenCalledWith(expect.any(Uint8Array));
    expect((random.mock.calls[0][0] as Uint8Array).length).toBe(25);
  });
});

describe("a passphrase's length", () => {
  it("counts characters of its NFC form, as the panel does", () => {
    const nfc = "Příliš žluťoučký kůň";
    const nfd = nfc.normalize("NFD");
    expect(nfd.length).toBeGreaterThan(nfc.length);
    expect(passphraseLength(nfd)).toBe(20);
    expect(passphraseLength(nfc)).toBe(20);
    // one character outside the BMP is one
    expect(passphraseLength("🔑".repeat(12))).toBe(12);
  });
});

describe("the recovery API", () => {
  it("reads the state, seals a kit with the sign-in password and checks one", async () => {
    const send = vi.spyOn(pb, "send").mockResolvedValue({});
    await recoveryApi.getRecovery();
    expect(send).toHaveBeenLastCalledWith("/api/pc/recovery", {});
    await recoveryApi.exportKit("correct horse battery", "hunter22");
    expect(send).toHaveBeenLastCalledWith("/api/pc/recovery/kit", {
      method: "POST",
      body: { passphrase: "correct horse battery", password: "hunter22" },
    });
    await recoveryApi.checkKit("-----BEGIN POCKETCLOUD RECOVERY KIT-----", "correct horse battery");
    expect(send).toHaveBeenLastCalledWith("/api/pc/recovery/kit/check", {
      method: "POST",
      body: {
        kit: "-----BEGIN POCKETCLOUD RECOVERY KIT-----",
        passphrase: "correct horse battery",
      },
    });
  });

  it("saves a kit as a text file under its name", async () => {
    const blobs: Blob[] = [];
    URL.createObjectURL = vi.fn((blob: Blob) => {
      blobs.push(blob);
      return "blob:kit";
    });
    URL.revokeObjectURL = vi.fn();
    const clicked: HTMLAnchorElement[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      clicked.push(this);
    });

    recoveryApi.saveTextFile("pocketcloud-recovery-kit-20261117-KIT-7Q2M-9XHC.pckit", "kit text");
    expect(clicked).toHaveLength(1);
    expect(clicked[0].download).toBe("pocketcloud-recovery-kit-20261117-KIT-7Q2M-9XHC.pckit");
    expect(clicked[0].href).toBe("blob:kit");
    expect(await blobs[0].text()).toBe("kit text");
    expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:kit");
  });
});
