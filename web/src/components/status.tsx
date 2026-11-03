import { useTranslation } from "react-i18next";

import type { CommandStatus, HostStatus } from "../lib/types";
import { StatusPill, type Tone } from "./ui";

export function hostTone(status: HostStatus | ""): Tone {
  switch (status) {
    case "online":
      return "ok";
    case "degraded":
      return "warn";
    case "offline":
      return "err";
    default:
      return "off";
  }
}

export function HostStatusPill({ status }: { status: HostStatus | "" }) {
  const { t } = useTranslation();
  return <StatusPill tone={hostTone(status)}>{t(`host.status.${status || "unknown"}`)}</StatusPill>;
}

export function commandTone(status: CommandStatus): Tone {
  switch (status) {
    case "succeeded":
      return "ok";
    case "failed":
    case "timed_out":
      return "err";
    case "cancelled":
      return "off";
    case "queued":
      return "off";
    default:
      return "info";
  }
}

export function CommandStatusPill({ status }: { status: CommandStatus }) {
  const { t } = useTranslation();
  return (
    <StatusPill tone={commandTone(status)} dot={false}>
      {t(`command.status.${status}`)}
    </StatusPill>
  );
}

export function containerTone(state: string, health: string): Tone {
  if (health === "unhealthy") {
    return "err";
  }
  switch (state) {
    case "running":
      return health === "starting" ? "warn" : "ok";
    case "restarting":
    case "paused":
      return "warn";
    case "dead":
      return "err";
    default:
      return "off";
  }
}

export function ContainerStatePill({ state, health }: { state: string; health: string }) {
  const { t } = useTranslation();
  const label =
    health === "unhealthy" ? t("container.unhealthy") : t(`container.state.${state}`, state);
  return <StatusPill tone={containerTone(state, health)}>{label}</StatusPill>;
}
