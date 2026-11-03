import type { RecordModel } from "pocketbase";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";

import { PageHead } from "../components/Layout";
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
