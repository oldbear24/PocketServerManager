import type { RecordModel } from "pocketbase";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";

import { AcmeAccountsCard } from "../components/AcmeAccounts";
import { PageHead } from "../components/Layout";
import { PhpVersionsCard } from "../components/PhpVersions";
import { useToast } from "../components/toast";
import { Button, Card, Field, SegmentedControl } from "../components/ui";
import { languages } from "../i18n";
import { api } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { pb } from "../lib/pb";
import type { PanelInfo } from "../lib/types";

type PanelSettings = RecordModel & {
  public_url: string;
  audit_retention_days: number;
  command_queue_ttl_s: number;
  smtp_host: string;
  smtp_port: number;
  smtp_tls: "" | "starttls" | "tls" | "off";
  smtp_user: string;
  smtp_password_set: boolean;
  smtp_from: string;
};

export function SettingsPage() {
  const { t, i18n } = useTranslation();
  const { user } = useAuth();

  return (
    <>
      <PageHead crumbs={t("nav.system")} title={t("nav.settings")} />

      <div className="columns">
        <div className="column-wide">
          {hasRole(user, "admin") && <PanelSettingsCard />}
          {hasRole(user, "admin") && <AcmeAccountsCard />}
          {hasRole(user, "admin") && <SmtpCard />}
          <PhpVersionsCard />
          <AboutCard />
        </div>
        <div className="column-side">
          <Card title={t("settings.language")}>
            <SegmentedControl
              label={t("settings.language")}
              value={i18n.resolvedLanguage ?? "en"}
              onChange={(language) => void i18n.changeLanguage(language)}
              options={languages.map((language) => ({
                value: language,
                label: t(`settings.languageName.${language}`),
              }))}
            />
          </Card>

          <Card title={t("settings.account")}>
            <dl className="facts">
              <dt>{t("auth.email")}</dt>
              <dd>{user?.email}</dd>
              <dt>{t("settings.role")}</dt>
              <dd>{t(`settings.roleName.${user?.role}`)}</dd>
            </dl>
          </Card>
        </div>
      </div>
    </>
  );
}

function PanelSettingsCard() {
  const { t } = useTranslation();
  const toast = useToast();

  const [record, setRecord] = useState<PanelSettings | null>(null);
  const [retention, setRetention] = useState("");
  const [queueTTL, setQueueTTL] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    pb.collection("settings")
      .getFirstListItem<PanelSettings>("")
      .then((loaded) => {
        if (!cancelled) {
          setRecord(loaded);
          setRetention(String(loaded.audit_retention_days));
          setQueueTTL(String(loaded.command_queue_ttl_s));
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          toast.error(err);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [toast]);

  if (!record) {
    return null;
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    try {
      const saved = await pb.collection("settings").update<PanelSettings>(record.id, {
        audit_retention_days: Number(retention),
        command_queue_ttl_s: Number(queueTTL),
      });
      setRecord(saved);
      toast.success(t("settings.saved"));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={t("settings.panel")}>
      <form onSubmit={(event) => void submit(event)} style={{ display: "grid", gap: 12 }}>
        <div className="form-grid">
          <Field label={t("settings.retention")} htmlFor="retention">
            <input
              id="retention"
              className="input"
              type="number"
              min={1}
              value={retention}
              onChange={(event) => setRetention(event.target.value)}
            />
          </Field>
          <Field label={t("settings.queueTTL")} htmlFor="queue-ttl">
            <input
              id="queue-ttl"
              className="input"
              type="number"
              min={60}
              value={queueTTL}
              onChange={(event) => setQueueTTL(event.target.value)}
            />
          </Field>
        </div>
        <div>
          <Button type="submit" variant="primary" disabled={busy}>
            {t("common.save")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/** The SMTP relay of PHP sites' mail() (#51): every running PHP site takes a
 * change at once. */
function SmtpCard() {
  const { t } = useTranslation();
  const toast = useToast();
  const [loaded, setLoaded] = useState(false);
  const [passwordSet, setPasswordSet] = useState(false);
  const [values, setValues] = useState({
    host: "",
    port: "587",
    tls: "starttls",
    user: "",
    password: "",
    from: "",
  });
  const [busy, setBusy] = useState(false);
  const set = (change: Partial<typeof values>) =>
    setValues((current) => ({ ...current, ...change }));

  useEffect(() => {
    let cancelled = false;
    pb.collection("settings")
      .getFirstListItem<PanelSettings>("")
      .then((record) => {
        if (!cancelled) {
          setValues({
            host: record.smtp_host ?? "",
            port: String(record.smtp_port || 587),
            tls: record.smtp_tls || "starttls",
            user: record.smtp_user ?? "",
            password: "",
            from: record.smtp_from ?? "",
          });
          setPasswordSet(Boolean(record.smtp_password_set));
          setLoaded(true);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          toast.error(err);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [toast]);

  if (!loaded) {
    return null;
  }

  const save = async (body: Record<string, unknown>) => {
    setBusy(true);
    try {
      const saved = await pb.send<{ password_set: boolean; applied: number }>(
        "/api/pc/settings/smtp",
        { method: "PUT", body },
      );
      setPasswordSet(saved.password_set);
      set({ password: "" });
      toast.success(t("settings.smtpSaved", { count: saved.applied }));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };
  const submit = (event: FormEvent) => {
    event.preventDefault();
    void save({
      host: values.host.trim(),
      port: Number(values.port),
      tls: values.tls,
      user: values.user.trim(),
      from: values.from.trim(),
      // an empty field keeps the password
      ...(values.password ? { password: values.password } : {}),
    });
  };

  return (
    <Card title={t("settings.smtp")} note={t("settings.smtpNote")}>
      <form onSubmit={submit} style={{ display: "grid", gap: 12 }}>
        <div className="form-grid">
          <Field label={t("settings.smtpHost")} htmlFor="smtp-host">
            <input
              id="smtp-host"
              className="input mono"
              placeholder="smtp.example.org"
              value={values.host}
              onChange={(event) => set({ host: event.target.value })}
            />
          </Field>
          <Field label={t("settings.smtpPort")} htmlFor="smtp-port">
            <input
              id="smtp-port"
              className="input"
              type="number"
              min={1}
              max={65535}
              value={values.port}
              onChange={(event) => set({ port: event.target.value })}
            />
          </Field>
          <Field label="TLS" htmlFor="smtp-tls">
            <select
              id="smtp-tls"
              className="select"
              value={values.tls}
              onChange={(event) => set({ tls: event.target.value })}
            >
              <option value="starttls">STARTTLS</option>
              <option value="tls">TLS</option>
              <option value="off">{t("settings.smtpTlsOff")}</option>
            </select>
          </Field>
          <Field label={t("settings.smtpFrom")} htmlFor="smtp-from">
            <input
              id="smtp-from"
              className="input mono"
              placeholder="noreply@example.org"
              value={values.from}
              onChange={(event) => set({ from: event.target.value })}
            />
          </Field>
          <Field label={t("settings.smtpUser")} htmlFor="smtp-user">
            <input
              id="smtp-user"
              className="input mono"
              autoComplete="off"
              value={values.user}
              onChange={(event) => set({ user: event.target.value })}
            />
          </Field>
          <Field label={t("settings.smtpPassword")} htmlFor="smtp-password">
            <input
              id="smtp-password"
              className="input mono"
              type="password"
              autoComplete="new-password"
              placeholder={passwordSet ? t("settings.smtpPasswordKept") : ""}
              value={values.password}
              onChange={(event) => set({ password: event.target.value })}
            />
          </Field>
        </div>
        <p className="muted small">{t("settings.smtpFromNote")}</p>
        <div className="button-row">
          <Button type="submit" variant="primary" disabled={busy}>
            {t("common.save")}
          </Button>
          {passwordSet && (
            <Button disabled={busy} onClick={() => void save({ password: "" })}>
              {t("settings.smtpRemovePassword")}
            </Button>
          )}
        </div>
      </form>
    </Card>
  );
}

function AboutCard() {
  const { t } = useTranslation();
  const [info, setInfo] = useState<PanelInfo | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .info()
      .then((loaded) => {
        if (!cancelled) {
          setInfo(loaded);
        }
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);

  if (!info) {
    return null;
  }

  return (
    <Card title={t("settings.about")}>
      <dl className="facts">
        <dt>{t("settings.version")}</dt>
        <dd className="mono">{info.version}</dd>
        <dt>{t("settings.gateway")}</dt>
        <dd className="mono">{info.grpc_address}</dd>
        <dt>CA_PIN</dt>
        <dd className="mono small">{info.ca_pin}</dd>
        <dt>{t("settings.rootCertificate")}</dt>
        <dd>
          <a href="/api/pc/ca.pem">{t("settings.rootCertificateDownload")}</a>
        </dd>
      </dl>
    </Card>
  );
}
