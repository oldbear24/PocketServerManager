import { TerminalSquare } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useParams } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { LogViewer } from "../components/LogViewer";
import { ContainerStatePill } from "../components/status";
import { EmptyState } from "../components/ui";
import { UsageChart } from "../components/UsageChart";
import { hasRole, useAuth } from "../lib/auth";
import { formatBytes } from "../lib/format";
import { useContainerMetrics, useContainerValues, type MetricRange } from "../lib/metrics";
import { pb } from "../lib/pb";
import { useLiveList, useLiveRecord } from "../lib/realtime";
import type { Container, Host } from "../lib/types";

/** One container: what it uses, what it writes, and a way in. */
export function ContainerDetailPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const { id = "", docker = "" } = useParams();

  const { record: host } = useLiveRecord<Host>("hosts", id);
  const containers = useLiveList<Container>("containers", {
    filter: pb.filter("host = {:host} && docker_id = {:docker}", { host: id, docker }),
    perPage: 1,
    enabled: Boolean(id && docker),
  });

  const [range, setRange] = useState<MetricRange>("1h");
  const metrics = useContainerMetrics(id, docker, range);
  const current = useContainerValues(id).data?.[docker];

  const container = containers.items[0];

  if (containers.loading) {
    return null;
  }

  if (!container) {
    return <EmptyState title={t("container.notFound")} />;
  }

  return (
    <>
      <PageHead
        crumbs={
          <>
            <Link to="/servers">{t("nav.servers")}</Link> /{" "}
            <Link to={`/servers/${id}/containers`}>{host?.name ?? id}</Link> / {container.name}
          </>
        }
        title={<span className="mono">{container.name}</span>}
        titleExtra={<ContainerStatePill state={container.state} health={container.health} />}
        description={<span className="mono">{container.image}</span>}
        actions={
          hasRole(user, "admin") && (
            <Link
              className="button"
              to={`/servers/${id}/terminal?container=${encodeURIComponent(docker)}`}
            >
              <TerminalSquare size={16} aria-hidden="true" />
              {t("terminal.open")}
            </Link>
          )
        }
      />

      <UsageChart
        title={t("server.usage")}
        points={metrics.data}
        stale={metrics.stale}
        range={range}
        onRange={setRange}
        currentCpu={current?.cpu ?? 0}
        currentMem={
          current ? `${formatBytes(current.mem)} / ${formatBytes(current.mem_limit)}` : "–"
        }
      />

      <LogViewer
        path={`/api/pc/hosts/${id}/logs?containers=${encodeURIComponent(docker)}`}
        fileName={`${container.name}.log`}
      />
    </>
  );
}
