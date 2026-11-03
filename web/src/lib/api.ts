import { pb } from "./pb";
import type {
  Command,
  DiagAgent,
  DiagAgentDetail,
  DiagOverview,
  NewEnrollmentToken,
  PanelInfo,
  VerifyResult,
} from "./types";

export const api = {
  info: () => pb.send<PanelInfo>("/api/pc/info", {}),

  setupNeeded: () => pb.send<{ needed: boolean }>("/api/pc/setup", {}),

  setup: (body: { token: string; email: string; name: string; password: string }) =>
    pb.send("/api/pc/setup", { method: "POST", body }),

  createEnrollmentToken: (name: string, roles: string[]) =>
    pb.send<NewEnrollmentToken>("/api/pc/enrollment-tokens", {
      method: "POST",
      body: { name, roles },
    }),

  /** Queues a command for a host. Everything else about it is up to the panel. */
  runCommand: (host: string, type: string, payload: object, label = "") =>
    pb.collection("commands").create<Command>({ host, type, payload, label }),

  cancelCommand: (id: string) => pb.send(`/api/pc/commands/${id}/cancel`, { method: "POST" }),

  updateHost: (id: string) => pb.send<Command>(`/api/pc/hosts/${id}/update`, { method: "POST" }),

  updateAllHosts: () =>
    pb.send<{ correlation_id: string; hosts: number }>("/api/pc/hosts/update-all", {
      method: "POST",
    }),

  verifyAudit: () => pb.send<VerifyResult>("/api/pc/audit/verify", { method: "POST" }),

  revertAudit: (id: string) => pb.send(`/api/pc/audit/${id}/revert`, { method: "POST" }),

  diagOverview: (window: string) =>
    pb.send<DiagOverview>(`/api/diag/overview?window=${window}`, {}),

  diagAgents: () => pb.send<DiagAgent[]>("/api/diag/agents", {}),

  diagAgent: (id: string) => pb.send<DiagAgentDetail>(`/api/diag/agents/${id}`, {}),

  diagCapture: (id: string) =>
    pb.send<{ until: string }>(`/api/diag/agents/${id}/capture`, { method: "POST" }),
};

/** Saves the response of an authenticated request as a file. */
export async function download(path: string, fallbackName: string) {
  const response = await fetch(path, {
    headers: { Authorization: pb.authStore.token },
  });

  if (!response.ok) {
    throw new Error((await response.json().catch(() => null))?.message ?? response.statusText);
  }

  const disposition = response.headers.get("Content-Disposition") ?? "";
  const name = /filename="([^"]+)"/.exec(disposition)?.[1] ?? fallbackName;

  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}

/**
 * Follows a server-sent event stream that needs authentication, which EventSource
 * cannot do. Returns when the signal aborts or the stream ends.
 */
export async function followEvents<T>(
  path: string,
  onMessage: (message: T) => void,
  signal: AbortSignal,
) {
  const response = await fetch(path, {
    headers: { Authorization: pb.authStore.token },
    signal,
  });

  if (!response.ok || !response.body) {
    throw new Error(response.statusText);
  }

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();

  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      return;
    }

    buffer += value;

    let boundary = buffer.indexOf("\n\n");
    while (boundary >= 0) {
      const chunk = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      boundary = buffer.indexOf("\n\n");

      for (const line of chunk.split("\n")) {
        if (line.startsWith("data: ")) {
          onMessage(JSON.parse(line.slice(6)) as T);
        }
      }
    }
  }
}

/** Returns the message of an error from the panel. */
export function errorMessage(error: unknown): string {
  if (error && typeof error === "object") {
    const fields = (error as { response?: { data?: Record<string, { message?: string }> } })
      .response?.data;
    const first = fields && Object.values(fields)[0]?.message;
    if (first) {
      return first;
    }
    if ("message" in error && typeof error.message === "string") {
      return error.message;
    }
  }
  return String(error);
}
