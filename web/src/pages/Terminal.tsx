import { lazy, Suspense, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams, useSearchParams } from "react-router-dom";

import { PageHead } from "../components/Layout";
import type { TerminalState } from "../components/TerminalView";
import { Banner, Button, Card, EmptyState, StatusPill, type Tone } from "../components/ui";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord } from "../lib/realtime";
import type { Container, Host } from "../lib/types";

const TerminalView = lazy(() => import("../components/TerminalView"));

const hostShellValue = "@host";

function stateTone(state: TerminalState): Tone {
  switch (state.kind) {
    case "open":
      return "ok";
    case "connecting":
      return "info";
    case "error":
      return "err";
    default:
      return "off";
  }
}

/** A shell in a container or, where that was allowed on the agent, on the host. */
export function TerminalPage() {
  const { t } = useTranslation();
  const { id = "" } = useParams();
  const [params, setParams] = useSearchParams();

  const { record: host, loading } = useLiveRecord<Host>("hosts", id);
  const containers = useLiveList<Container>("containers", {
    filter: pb.filter("host = {:host} && state = 'running'", { host: id }),
    sort: "name",
    enabled: Boolean(id),
  });

  const target = params.get("container") ?? (params.get("host") ? hostShellValue : "");

  const [session, setSession] = useState(0);
  const [connected, setConnected] = useState(Boolean(target));
  const [state, setState] = useState<TerminalState>({ kind: "connecting" });

  if (loading) {
    return null;
  }

  if (!host) {
    return <EmptyState title={t("server.notFound")} />;
  }

  const choose = (value: string) => {
    setConnected(false);
    setState({ kind: "connecting" });
    if (value === hostShellValue) {
      setParams({ host: "1" });
    } else if (value) {
      setParams({ container: value });
    } else {
      setParams({});
    }
  };

  const connect = () => {
    setSession((current) => current + 1);
    setState({ kind: "connecting" });
    setConnected(true);
  };

  const isHostShell = target === hostShellValue;
  const over = !["connecting", "open"].includes(state.kind);

  const stateText =
    state.kind === "exited"
      ? t("terminal.state.exited", { code: state.code })
      : state.kind === "error"
        ? t("terminal.state.error")
        : t(`terminal.state.${state.kind}`);

  return (
    <>
      <PageHead
        crumbs={
          <>
            <Link to="/servers">{t("nav.servers")}</Link> /{" "}
            <Link to={`/servers/${host.id}`}>{host.name}</Link> / {t("terminal.open")}
          </>
        }
        title={t("terminal.title")}
        description={t("terminal.note")}
        titleExtra={connected && <StatusPill tone={stateTone(state)}>{stateText}</StatusPill>}
      />

      <Card>
        <div className="filters">
          <label className="field-label" htmlFor="terminal-target">
            {t("terminal.target")}
          </label>
          <select
            id="terminal-target"
            className="select"
            value={target}
            onChange={(event) => choose(event.target.value)}
          >
            <option value="">{t("terminal.choose")}</option>
            {containers.items.map((container) => (
              <option key={container.id} value={container.docker_id}>
                {container.name}
              </option>
            ))}
            {host.host_shell_enabled && (
              <option value={hostShellValue}>{t("terminal.hostShell")}</option>
            )}
          </select>
          <Button
            variant="primary"
            disabled={!target || host.status === "offline" || (connected && !over)}
            onClick={connect}
          >
            {connected ? t("terminal.reconnect") : t("terminal.connect")}
          </Button>
        </div>

        {isHostShell && (
          <div style={{ marginTop: 12 }}>
            <Banner tone="warn">{t("terminal.hostShellRisk")}</Banner>
          </div>
        )}
        {!host.host_shell_enabled && (
          <p className="muted small" style={{ marginTop: 12 }}>
            {t("terminal.hostShellOff")}
          </p>
        )}
      </Card>

      {state.kind === "error" && <Banner tone="err">{state.message}</Banner>}

      {connected && target && (
        <div className="terminal-frame">
          <Suspense fallback={<div className="log-empty">{t("terminal.loading")}</div>}>
            <TerminalView
              key={`${target}/${session}`}
              host={host.id}
              target={isHostShell ? { host_shell: true } : { container: target }}
              onState={setState}
            />
          </Suspense>
        </div>
      )}
    </>
  );
}
