import { Download } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { errorMessage } from "../lib/api";
import { caApi, type InternalCA } from "../lib/certificates";
import { formatDate } from "../lib/format";
import { Banner, Card, CodeBlock, Field } from "./ui";

/** The devices the install steps are written for. */
export const platforms = ["windows", "macos", "linux", "ios", "android", "firefox"] as const;
export type Platform = (typeof platforms)[number];

/** How many steps each device has in the translations. */
const steps: Record<Platform, number> = {
  windows: 4,
  macos: 3,
  linux: 2,
  ios: 3,
  android: 2,
  firefox: 2,
};

/** What a terminal does instead, where there is one. */
const commands: Partial<Record<Platform, string>> = {
  windows:
    "Import-Certificate -FilePath .\\pocketcloud-root-ca.crt -CertStoreLocation Cert:\\LocalMachine\\Root",
  macos:
    "sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain pocketcloud-root-ca.crt",
  linux: [
    "# Debian, Ubuntu",
    "sudo cp pocketcloud-root-ca.pem /usr/local/share/ca-certificates/pocketcloud-root-ca.crt",
    "sudo update-ca-certificates",
    "# Fedora, RHEL",
    "sudo cp pocketcloud-root-ca.pem /etc/pki/ca-trust/source/anchors/ && sudo update-ca-trust",
    "# Chrome and Chromium keep their own list on Linux",
    'certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n "PocketCloud Root CA" -i pocketcloud-root-ca.pem',
  ].join("\n"),
};

/** The device the page is opened on, to show its steps first. */
export function guessPlatform(userAgent: string): Platform {
  if (/Firefox\//.test(userAgent)) {
    return "firefox";
  }
  if (/iPhone|iPad|iPod/.test(userAgent)) {
    return "ios";
  }
  if (/Android/.test(userAgent)) {
    return "android";
  }
  if (/Mac OS X|Macintosh/.test(userAgent)) {
    return "macos";
  }
  if (/Windows/.test(userAgent)) {
    return "windows";
  }
  return "linux";
}

/** The internal CA (#26): its root, what it signs, the download of the root
 * and how to install it on each kind of device, so that LAN names open
 * over HTTPS without a warning. */
export function InternalCACard() {
  const { t } = useTranslation();
  const [ca, setCA] = useState<InternalCA | null>(null);
  const [error, setError] = useState("");
  const [platform, setPlatform] = useState<Platform>(() => guessPlatform(navigator.userAgent));

  useEffect(() => {
    let cancelled = false;
    caApi
      .info()
      .then((info) => {
        if (!cancelled) {
          setCA(info);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(errorMessage(err));
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const command = commands[platform];

  return (
    <Card title={t("ca.title")}>
      {error ? (
        <Banner tone="err">{error}</Banner>
      ) : !ca ? (
        <p className="muted">{t("common.loading")}</p>
      ) : (
        <p className="ca-summary">
          {t("ca.summary", {
            subject: ca.root.subject,
            date: formatDate(ca.root.not_after),
            agents: ca.agents,
            lan: ca.lan_certificates,
          })}
        </p>
      )}
      <div className="row-actions ca-actions">
        <a className="button" href={caApi.rootURL("crt")} download="pocketcloud-root-ca.crt">
          <Download size={14} aria-hidden="true" /> {t("ca.download")}
        </a>
        <a
          className="button"
          href={caApi.rootURL("pem")}
          download="pocketcloud-root-ca.pem"
          aria-label={t("ca.downloadPemLabel")}
        >
          PEM
        </a>
        <Link className="button" to="/agents">
          {t("ca.agents")}
        </Link>
      </div>
      <details className="ca-install">
        <summary>{t("ca.install")}</summary>
        <div className="dialog-form">
          <p className="muted small">{t("ca.warning")}</p>
          {ca && (
            <div>
              <div className="small muted">{t("ca.fingerprint")}</div>
              <code className="mono small ca-fingerprint">{ca.root.sha256}</code>
              <p className="muted small">{t("ca.fingerprintNote")}</p>
            </div>
          )}
          <Field label={t("ca.device")} htmlFor="ca-device">
            <select
              id="ca-device"
              className="input"
              value={platform}
              onChange={(event) => setPlatform(event.target.value as Platform)}
            >
              {platforms.map((id) => (
                <option key={id} value={id}>
                  {t(`ca.platform.${id}`)}
                </option>
              ))}
            </select>
          </Field>
          <ol className="ca-steps">
            {Array.from({ length: steps[platform] }, (_, i) => (
              <li key={i}>{t(`ca.steps.${platform}.${i + 1}`)}</li>
            ))}
          </ol>
          {command && <CodeBlock code={command} label={t("ca.command")} />}
          <p className="muted small">{t("ca.after")}</p>
        </div>
      </details>
    </Card>
  );
}
