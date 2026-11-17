import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";

import { CodeBlock } from "../components/ui";
import { api } from "../lib/api";
import type { BackupSnapshot } from "../lib/backupRestores";
import type { BackupTarget } from "../lib/backups";
import { dialAddress, panelImage, recoveryCommands } from "../lib/recovery";
import type { Host, PanelInfo } from "../lib/types";

/** The agent's image when the panel does not say it. */
const defaultAgentImage = "ghcr.io/oldbear24/pocketcloud-agent:latest";

/** How the panel comes back from one of its snapshots (#32): not from
 * inside itself, but on a machine of its own where the servers dial it,
 * with the recovery kit. Each step's commands are filled from the snapshot,
 * the target, the panel's images and the address the servers dial. */
export function PanelRecoverySteps({
  target,
  snapshot,
  hosts,
}: {
  target: BackupTarget | null;
  snapshot: Pick<BackupSnapshot, "short_id"> | null;
  hosts: Host[];
}) {
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

  const address = dialAddress(hosts, info?.grpc_address ?? "");
  const commands = recoveryCommands({
    snapshot: snapshot?.short_id || "latest",
    target: target?.name ?? "",
    agentImage: info?.agent_image || defaultAgentImage,
    panelImage: panelImage(info?.agent_image ?? "", info?.version ?? ""),
    address,
  });
  const steps = [
    { title: t("backups.restore.panel.step1"), code: commands.fetch },
    { title: t("backups.restore.panel.step2"), code: commands.restore },
    { title: t("backups.restore.panel.step3"), code: commands.start },
  ];

  return (
    <section className="panel-recovery" aria-labelledby="panel-recovery-title">
      <h3 id="panel-recovery-title" className="drawer-heading">
        {t("backups.restore.panel.title")}
      </h3>
      <p>{t("backups.restore.panel.text", { address: address || "–" })}</p>
      <ol className="panel-recovery-steps">
        {steps.map((step, index) => (
          <li key={step.title}>
            <h4>{step.title}</h4>
            <CodeBlock code={step.code} label={step.title} />
            {index === 0 && (
              <>
                <p className="muted small">{t("backups.restore.panel.launcher")}</p>
                <CodeBlock code={commands.launcher} label={t("backups.restore.panel.launcher")} />
              </>
            )}
          </li>
        ))}
        <li>
          <h4>{t("backups.restore.panel.step4")}</h4>
          <Link to="/settings#recovery-kit">{t("settings.recovery.title")}</Link>
        </li>
      </ol>
      <p className="muted small">{t("backups.restore.panel.guide")}</p>
    </section>
  );
}
