import type { RecordModel } from "pocketbase";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { AcmeAccountsCard } from "../components/AcmeAccounts";
import { PageHead } from "../components/Layout";
import { PhpVersionsCard } from "../components/PhpVersions";
import { RecoveryCard } from "../components/RecoveryCard";
import { useToast } from "../components/toast";
import { Banner, Button, Card, Field, SegmentedControl } from "../components/ui";
import { languages } from "../i18n";
import { api } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import {
  defaultGroups,
  maxGroups,
  parseGroups,
  saveCheckSettings,
  type CheckSettingsRecord,
} from "../lib/backupChecks";
import { cronShaped, describeCron, zoneOffset } from "../lib/backupJobs";
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
          {hasRole(user, "admin") && <RecoveryCard />}
          {hasRole(user, "admin") && <AcmeAccountsCard />}
          {hasRole(user, "admin") && <SmtpCard />}
          {hasRole(user, "admin") && <BackupChecksCard />}
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
              required={values.host.trim() !== ""}
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

/** When the backups' repositories are checked (#31): the schedule, cron in
 * UTC with its local reading, and how many groups the data packs are read
 * in. An empty schedule turns the checks off, which the card warns of. */
export function BackupChecksCard() {
  const { t } = useTranslation();
  const toast = useToast();
  const [loaded, setLoaded] = useState(false);
  const [schedule, setSchedule] = useState("");
  const [groups, setGroups] = useState(String(defaultGroups));
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    pb.collection("settings")
      .getFirstListItem<CheckSettingsRecord>("")
      .then((record) => {
        if (!cancelled) {
          setSchedule(record.backup_check_schedule ?? "");
          setGroups(String(record.backup_check_groups ?? 0));
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

  const parsed = parseGroups(groups);
  const off = schedule.trim() === "";
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (parsed === null) {
      return;
    }
    setBusy(true);
    try {
      const saved = await saveCheckSettings({ schedule: schedule.trim(), groups: parsed });
      setSchedule(saved.schedule);
      setGroups(String(saved.groups));
      toast.success(t("settings.backupChecksSaved"));
    } catch (err) {
      toast.error(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card title={t("settings.backupChecks")} note={t("settings.backupChecksNote")}>
      <form onSubmit={(event) => void submit(event)} style={{ display: "grid", gap: 12 }}>
        <div className="form-grid">
          <Field label={t("settings.backupCheckSchedule")} htmlFor="backup-check-schedule">
            <input
              id="backup-check-schedule"
              className="input mono"
              autoComplete="off"
              spellCheck={false}
              placeholder="0 5 * * 0"
              aria-describedby="backup-check-schedule-hint"
              value={schedule}
              onChange={(event) => setSchedule(event.target.value)}
            />
            <p id="backup-check-schedule-hint" className="muted small">
              {t("settings.backupCheckScheduleHint")}
              {!off &&
                cronShaped(schedule) &&
                ` ${t("backups.job.cronReads", {
                  text: describeCron(schedule.trim(), t, zoneOffset()),
                })}`}
            </p>
          </Field>
          <Field
            label={t("settings.backupCheckGroups")}
            htmlFor="backup-check-groups"
            error={parsed === null ? t("settings.backupCheckGroupsInvalid") : undefined}
          >
            <input
              id="backup-check-groups"
              className="input"
              type="number"
              min={0}
              max={maxGroups}
              step={1}
              aria-invalid={parsed === null}
              aria-describedby="backup-check-groups-hint"
              value={groups}
              onChange={(event) => setGroups(event.target.value)}
            />
            <p id="backup-check-groups-hint" className="muted small">
              {t("settings.backupCheckGroupsHint")}
            </p>
          </Field>
        </div>
        {off && <Banner tone="warn">{t("settings.backupChecksOff")}</Banner>}
        <div>
          <Button type="submit" variant="primary" disabled={busy || parsed === null}>
            {t("common.save")}
          </Button>
        </div>
      </form>
    </Card>
  );
}

/** The panel itself: its version, HTTPS, gateway and CA. */
export function AboutCard() {
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
        <dt>{t("settings.https")}</dt>
        <dd>
          {info.https ? (
            info.panel_certificate ? (
              <Link to={`/domains?certificate=${info.panel_certificate}`}>
                {t("settings.httpsCertificate")}
              </Link>
            ) : (
              t("settings.httpsOrdering")
            )
          ) : (
            t("settings.httpsOff")
          )}
        </dd>
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
