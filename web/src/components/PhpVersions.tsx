import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { formatDate } from "../lib/format";
import { phpApi, phpNeedsWarning, type PhpState, type PhpVersion } from "../lib/php";
import { useToast } from "./toast";
import { Banner, Card, StatusPill, Table, type Tone } from "./ui";

const tones: Record<PhpState, Tone> = {
  supported: "ok",
  security: "info",
  ending: "warn",
  ended: "err",
};

export function PhpStatePill({ state }: { state: PhpState }) {
  const { t } = useTranslation();
  return <StatusPill tone={tones[state]}>{t(`php.state.${state}`)}</StatusPill>;
}

/** The warning for a version whose security support ends soon or has ended. */
export function PhpEolWarning({ version }: { version: PhpVersion }) {
  const { t } = useTranslation();
  if (!phpNeedsWarning(version)) {
    return null;
  }

  const date = formatDate(version.security_support_until);
  return (
    <Banner tone={version.state === "ended" ? "err" : "warn"}>
      {version.state === "ended"
        ? t("php.endedWarning", { version: version.version, date })
        : t("php.endingWarning", { version: version.version, date, count: version.days_left })}
    </Banner>
  );
}

/** The PHP versions that PHP sites can run, with the dates their support ends. */
export function PhpVersionsCard() {
  const { t } = useTranslation();
  const toast = useToast();
  const [versions, setVersions] = useState<PhpVersion[] | null>(null);

  useEffect(() => {
    let cancelled = false;
    phpApi
      .versions()
      .then((loaded) => {
        if (!cancelled) {
          setVersions(loaded.versions);
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

  if (!versions) {
    return null;
  }

  return (
    <Card title={t("php.title")} note={t("php.note")}>
      <div style={{ display: "grid", gap: 12 }}>
        {versions.filter(phpNeedsWarning).map((version) => (
          <PhpEolWarning key={version.version} version={version} />
        ))}
        <Table label={t("php.title")} compact>
          <thead>
            <tr>
              <th>{t("php.version")}</th>
              <th>{t("php.support")}</th>
              <th>{t("php.activeUntil")}</th>
              <th>{t("php.securityUntil")}</th>
              <th>{t("php.image")}</th>
            </tr>
          </thead>
          <tbody>
            {versions.map((version) => (
              <tr key={version.version}>
                <td>PHP {version.version}</td>
                <td>
                  <PhpStatePill state={version.state} />
                </td>
                <td>{formatDate(version.active_support_until)}</td>
                <td>{formatDate(version.security_support_until)}</td>
                <td className="mono small">{version.image}</td>
              </tr>
            ))}
          </tbody>
        </Table>
      </div>
    </Card>
  );
}
