import { pb } from "./pb";

/** How the newest recovery kit stands against the panel as it is now (#32):
 * none yet, downloaded but no check opened it, out of date, or checked and
 * current. Only `saved` ends the reminder. */
export type KitStatus = "never" | "unverified" | "stale" | "saved";

/** What a kit holds that can go out of date, in the order the panel names
 * them. */
export type KitSection = "key" | "address" | "job" | "targets";

export const kitSections: KitSection[] = ["key", "address", "job", "targets"];

/** An address the servers dial the panel at, and how many dial it; `ip` when
 * it is an IP address, which a new machine must take over. */
export type DialAddress = { address: string; servers: number; ip: boolean };

/** What Settings › Recovery kit shows: the master key's id and the CA's pin,
 * the newest kit, where the servers dial, how to start a restored panel, the
 * panel's backup job, and a restore the panel came back from. Admins only. */
export type RecoveryState = {
  key_id: string;
  ca_pin: string;
  kit: {
    status: KitStatus;
    /** of a stale kit, the sections that changed */
    reasons: KitSection[] | null;
    kit_id?: string;
    created?: string;
    created_by?: string;
    verified_at?: string;
    verified_by?: string;
  };
  addresses: DialAddress[] | null;
  /** servers that have not said yet which address they dial */
  unreported: number;
  /** the environment a restored panel starts with, never a secret */
  start_env: Record<string, string> | null;
  panel_job: {
    id: string;
    name: string;
    enabled: boolean;
    /** the server whose agent uploads it */
    carrier: string;
    carrier_name: string;
    /** the targets' names */
    targets: string[] | null;
    last_success_at: string;
  } | null;
  /** the restore the panel came back from, and how many servers are back */
  restored: {
    at: string;
    /** when the backup was made */
    from: string;
    snapshot: string;
    back: number;
    total: number;
  } | null;
};

/** A sealed kit, to save as a file at once: the panel keeps none of it. */
export type ExportedKit = { kit_id: string; file_name: string; content: string };

/** What a check of a kit found: it opened, and whether it is current. */
export type KitCheck = {
  kit_id: string;
  current: boolean;
  reasons: KitSection[] | null;
  status: KitStatus;
};

/** The longest kit the panel takes back to check. */
export const maxKitBytes = 64 * 1024;

/** The shortest passphrase the panel seals a kit with, in characters. */
export const minPassphrase = 12;

/** How many characters a passphrase has, as the panel counts them: code
 * points of its NFC form. */
export function passphraseLength(passphrase: string): number {
  return [...passphrase.normalize("NFC")].length;
}

/** Crockford's base32, as the kit ids are written: no I, L, O or U. */
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

/** A passphrase to write down: 25 characters of Crockford's base32 from the
 * browser's random numbers, 125 bits, in groups of five. A byte's lowest 5
 * bits are as random as the byte, so no character is likelier than another. */
export function generatePassphrase(random: (bytes: Uint8Array) => void = fill): string {
  const bytes = new Uint8Array(25);
  random(bytes);
  const code = Array.from(bytes, (byte) => crockford[byte & 31]).join("");
  return code.match(/.{5}/g)!.join("-");
}

function fill(bytes: Uint8Array) {
  crypto.getRandomValues(bytes);
}

/** The one file of a panel snapshot, which `panel recover` takes (#32). */
export const panelFileName = "pocketcloud-panel.pcpb";

/** A word as a shell takes it: as it is when it is plain, else in single
 * quotes. */
export function shellWord(word: string): string {
  return /^[\w@%+=:,./-]+$/.test(word) ? word : `'${word.replace(/'/g, `'\\''`)}'`;
}

/** The panel's image of this release, beside the agent's: the agent's
 * repository with pocketcloud-panel for its name, tagged with the version,
 * or latest for a development build. */
export function panelImage(agentImage: string, version: string): string {
  const repository = /^(.*\/)?pocketcloud-agent(?=[:@]|$)/.exec(agentImage);
  const base = repository
    ? `${repository[1] ?? ""}pocketcloud-panel`
    : "ghcr.io/oldbear24/pocketcloud-panel";
  const tag = version && version !== "dev" ? version : "latest";
  return `${base}:${tag}`;
}

/** The address most servers dial the panel at, as their streams said it;
 * else the panel's own idea of it. */
export function dialAddress(hosts: { panel_address?: string }[], fallback: string): string {
  const counts = new Map<string, number>();
  for (const host of hosts) {
    if (host.panel_address) {
      counts.set(host.panel_address, (counts.get(host.panel_address) ?? 0) + 1);
    }
  }
  let best = "";
  for (const [address, count] of counts) {
    if (best === "" || count > (counts.get(best) ?? 0)) {
      best = address;
    }
  }
  return best || fallback;
}

/** The port of an address such as 192.0.2.2:9443 or [2001:db8::2]:9443. */
function portOf(address: string): string {
  return /:(\d{1,5})$/.exec(address)?.[1] ?? "9443";
}

/** The commands of a panel's restore on a new machine, as the guide gives
 * them: fetch the snapshot with an agent (on a server, or through the
 * agent's image on the new machine), restore it into the new panel's
 * volumes, then check it and start the panel where the servers dial it.
 * The kit's passphrase is read without an echo, never written down. Lines
 * are short, as a drawer on a phone shows them. */
export function recoveryCommands(values: {
  /** a snapshot's short id, or latest */
  snapshot: string;
  target: string;
  agentImage: string;
  panelImage: string;
  address: string;
}): { fetch: string; launcher: string; restore: string; start: string } {
  const passphrase = "read -rs POCKETCLOUD_KIT_PASSPHRASE && export POCKETCLOUD_KIT_PASSPHRASE";
  const fetch = [
    "panel-recovery fetch --kit - \\",
    `  --target ${shellWord(values.target)} --snapshot ${shellWord(values.snapshot)} --out - \\`,
    `  < kit.pckit > ${panelFileName}`,
  ];
  const image = shellWord(values.panelImage);
  return {
    fetch: [
      passphrase,
      "docker exec -i -e POCKETCLOUD_KIT_PASSPHRASE pocketcloud-agent \\",
      `  pocketcloud-agent ${fetch.join("\n")}`,
    ].join("\n"),
    launcher: [
      passphrase,
      "docker run --rm -i -e POCKETCLOUD_KIT_PASSPHRASE \\",
      "  -v /var/run/docker.sock:/var/run/docker.sock \\",
      `  ${shellWord(values.agentImage)} \\`,
      `  ${fetch.join("\n")}`,
    ].join("\n"),
    restore: [
      "docker run --rm -it -v pocketcloud:/data \\",
      '  -v pocketcloud-secrets:/run/secrets -v "$PWD":/work \\',
      `  ${image} recover \\`,
      `  --kit /work/kit.pckit --from /work/${panelFileName} \\`,
      "  --env-out /work/panel.env",
    ].join("\n"),
    start: [
      "docker run --rm -v pocketcloud:/data \\",
      "  -v pocketcloud-secrets:/run/secrets:ro \\",
      `  ${image} audit verify`,
      "docker run -d --name pocketcloud-panel --restart unless-stopped \\",
      `  --env-file panel.env -p 8090:8090 -p ${portOf(values.address)}:9443 \\`,
      "  -v pocketcloud:/data -v pocketcloud-secrets:/run/secrets:ro \\",
      `  ${image}`,
    ].join("\n"),
  };
}

export const recoveryApi = {
  getRecovery: () => pb.send<RecoveryState>("/api/pc/recovery", {}),

  /** seals a kit: needs the admin's sign-in password; the answer is never kept */
  exportKit: (passphrase: string, password: string) =>
    pb.send<ExportedKit>("/api/pc/recovery/kit", {
      method: "POST",
      body: { passphrase, password },
    }),

  /** opens a kit as a recovery would, and marks it saved when it is current */
  checkKit: (kit: string, passphrase: string) =>
    pb.send<KitCheck>("/api/pc/recovery/kit/check", {
      method: "POST",
      body: { kit, passphrase },
    }),

  /** saves a text as a file of the browser's downloads, as `download` does
   * an answer of the panel */
  saveTextFile: (name: string, content: string) => {
    const url = URL.createObjectURL(new Blob([content], { type: "text/plain;charset=utf-8" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = name;
    link.click();
    URL.revokeObjectURL(url);
  },
};
