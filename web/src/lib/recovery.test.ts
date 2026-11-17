import { afterEach, describe, expect, it, vi } from "vitest";

import { pb } from "./pb";
import { generatePassphrase, passphraseLength, recoveryApi } from "./recovery";

afterEach(() => {
  vi.restoreAllMocks();
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
