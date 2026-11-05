import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useLocation } from "react-router-dom";

import { errorMessage } from "../lib/api";
import {
  certificatesApi,
  directories,
  directoryId,
  type AcmeAccount,
  type DirectoryId,
} from "../lib/certificates";
import { formatRelative } from "../lib/format";
import { useLiveList } from "../lib/realtime";
import { useToast } from "./toast";
import { Banner, Button, Card, ConfirmDialog, Field, StatusPill } from "./ui";

/** The ACME accounts, in Settings: the CA certificates come from, and the
 * contact email it sends notices to. Admins set them before the first
 * certificate. */
export function AcmeAccountsCard() {
  const { t } = useTranslation();
  const accounts = useLiveList<AcmeAccount>("acme_accounts", { sort: "role" });
  const primary = accounts.items.find((account) => account.role === "primary");
  const fallback = accounts.items.find((account) => account.role === "fallback");
  const { hash } = useLocation();

  // the router does not scroll to the anchor of a link from another page
  useEffect(() => {
    if (hash === "#acme") {
      document.getElementById("acme")?.scrollIntoView?.({ block: "start" });
    }
  }, [hash]);

  return (
    <Card id="acme" title={t("acme.title")} note={t("acme.note")}>
      {accounts.loading && accounts.items.length === 0 ? (
        <p className="muted">{t("common.loading")}</p>
      ) : (
        <div className="acme-accounts">
          <AccountForm key={primary?.id ?? "primary"} role="primary" account={primary} />
          <AccountForm key={fallback?.id ?? "fallback"} role="fallback" account={fallback} />
        </div>
      )}
    </Card>
  );
}

function AccountForm({ role, account }: { role: AcmeAccount["role"]; account?: AcmeAccount }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [ca, setCA] = useState<DirectoryId>(
    account ? directoryId(account.directory_url) : role === "primary" ? "letsencrypt" : "zerossl",
  );
  const [custom, setCustom] = useState(
    account && directoryId(account.directory_url) === "custom" ? account.directory_url : "",
  );
  const [email, setEmail] = useState(account?.email ?? "");
  const [kid, setKid] = useState(account?.eab_kid ?? "");
  const [hmac, setHmac] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [removing, setRemoving] = useState(false);
  const [editing, setEditing] = useState(!account && role === "primary");

  useEffect(() => setError(""), [ca]);

  const known = directories.find((directory) => directory.id === ca);
  const withEAB = ca === "custom" || Boolean(known?.eab);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await certificatesApi.setAccount(role, {
        directory_url: known ? known.url : custom.trim(),
        email: email.trim(),
        eab_kid: withEAB ? kid.trim() : "",
        eab_hmac: withEAB ? hmac.trim() : "",
      });
      setHmac("");
      setEditing(false);
      toast.success(t("acme.saved"));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    try {
      await certificatesApi.removeAccount(role);
      toast.success(t("acme.removed"));
    } catch (err) {
      toast.error(err);
    }
    setRemoving(false);
  };

  const id = (field: string) => `acme-${role}-${field}`;

  return (
    <section className="acme-account" aria-labelledby={id("title")}>
      <div className="acme-account-head">
        <h3 id={id("title")}>{t(`acme.role.${role}`)}</h3>
        {account && (
          <StatusPill tone="ok">
            {t("acme.registered", { when: formatRelative(account.registered_at) })}
          </StatusPill>
        )}
      </div>
      <p className="muted small">{t(`acme.roleNote.${role}`)}</p>

      {!editing ? (
        <div className="row-actions start">
          <Button onClick={() => setEditing(true)}>
            {account ? t("acme.change") : t("acme.add")}
          </Button>
          {account && (
            <Button variant="danger" onClick={() => setRemoving(true)}>
              {t("common.remove")}
            </Button>
          )}
        </div>
      ) : (
        <form onSubmit={(event) => void submit(event)} className="dialog-form">
          <Field label={t("acme.ca")} htmlFor={id("ca")}>
            <select
              id={id("ca")}
              className="input"
              value={ca}
              onChange={(event) => setCA(event.target.value as DirectoryId)}
            >
              {directories.map((directory) => (
                <option key={directory.id} value={directory.id}>
                  {t(`cert.ca.${directory.id}`)}
                </option>
              ))}
              <option value="custom">{t("acme.custom")}</option>
            </select>
          </Field>
          {ca === "letsencrypt-staging" && <p className="muted small">{t("acme.stagingNote")}</p>}
          {ca === "custom" && (
            <Field label={t("acme.directory")} htmlFor={id("directory")}>
              <input
                id={id("directory")}
                className="input mono"
                type="url"
                placeholder="https://acme.example.com/directory"
                value={custom}
                onChange={(event) => setCustom(event.target.value)}
              />
            </Field>
          )}
          <Field label={t("acme.email")} htmlFor={id("email")}>
            <input
              id={id("email")}
              className="input"
              type="email"
              autoComplete="email"
              required
              value={email}
              onChange={(event) => setEmail(event.target.value)}
            />
          </Field>
          {withEAB && (
            <>
              <p className="muted small">{t("acme.eabNote")}</p>
              <div className="form-grid">
                <Field label={t("acme.eabKid")} htmlFor={id("kid")}>
                  <input
                    id={id("kid")}
                    className="input mono"
                    autoComplete="off"
                    value={kid}
                    onChange={(event) => setKid(event.target.value)}
                  />
                </Field>
                <Field label={t("acme.eabHmac")} htmlFor={id("hmac")}>
                  <input
                    id={id("hmac")}
                    className="input mono"
                    type="password"
                    autoComplete="off"
                    placeholder={
                      account?.eab_kid && kid === account.eab_kid ? t("acme.hmacKept") : ""
                    }
                    value={hmac}
                    onChange={(event) => setHmac(event.target.value)}
                  />
                </Field>
              </div>
            </>
          )}
          <p className="muted small">{t("acme.terms")}</p>
          {error && <Banner tone="err">{error}</Banner>}
          <div className="row-actions start">
            <Button type="submit" variant="primary" disabled={busy}>
              {busy ? t("acme.registering") : t("acme.save")}
            </Button>
            {(account || role === "fallback") && (
              <Button onClick={() => setEditing(false)} disabled={busy}>
                {t("common.cancel")}
              </Button>
            )}
          </div>
        </form>
      )}

      <ConfirmDialog
        open={removing}
        title={t("acme.removeTitle")}
        confirmLabel={t("common.remove")}
        danger
        onCancel={() => setRemoving(false)}
        onConfirm={() => void remove()}
      >
        <p className="muted">{t(`acme.removeText.${role}`)}</p>
      </ConfirmDialog>
    </section>
  );
}
