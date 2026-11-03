import PocketBase, { type RecordModel } from "pocketbase";

export const pb = new PocketBase(window.location.origin);

// One screen often loads the same collection from several components.
pb.autoCancellation(false);

let pendingNote = "";

/** Attaches a note to the audit entry of the next change saved from the UI. */
export function setAuditNote(note: string) {
  pendingNote = note;
}

pb.beforeSend = (url, options) => {
  const headers: Record<string, string> = {
    ...(options.headers as Record<string, string> | undefined),
    "X-PocketCloud-Client": "web",
  };

  if (pendingNote && options.method && options.method !== "GET") {
    headers["X-PocketCloud-Note"] = encodeURIComponent(pendingNote);
    pendingNote = "";
  }

  return { url, options: { ...options, headers } };
};

export type Role = "admin" | "operator" | "viewer";

export type User = RecordModel & {
  email: string;
  name: string;
  role: Role;
};

export function currentUser(): User | null {
  return pb.authStore.isValid ? (pb.authStore.record as User | null) : null;
}
