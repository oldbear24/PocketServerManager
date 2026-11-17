import { History, RefreshCw } from "lucide-react";
import { ClientResponseError } from "pocketbase";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import {
  Banner,
  Button,
  Card,
  EmptyState,
  IconButton,
  SegmentedControl,
  StatusPill,
} from "../components/ui";
import { errorMessage } from "../lib/api";
import { formatMoment, type BackupJob, type BackupRun } from "../lib/backupJobs";
import {
  formatSnapshotTime,
  restoreOpen,
  restoresApi,
  restoreTone,
  useListing,
  type BackupRestore,
  type BackupSnapshot,
  type SnapshotTarget,
} from "../lib/backupRestores";
import type { BackupTarget } from "../lib/backups";
import { formatBytes } from "../lib/format";
import type { Host } from "../lib/types";

/** The snapshots shown at first; the older ones a click away. */
const firstRows = 20;

/** A job's snapshots in one target, newest first, with a switch between the
 * job's targets: snapshot ids belong to one repository. Operators browse
 * them and restore them. */
export function SnapshotsCard({
  job,
  sources,
  target,
  hosts,
  runs,
  now,
  canRestore,
  open,
  onTarget,
  onRestore,
  onFiles,
  onShowRestore,
}: {
  job: BackupJob;
  sources: SnapshotTarget[];
  target: BackupTarget | null;
  hosts: Host[];
  runs: Pick<BackupRun, "job" | "snapshot_id" | "total_bytes" | "bytes_added">[];
  now: number;
  canRestore: boolean;
  /** a restore of the job that was started and has not ended */
  open: BackupRestore | null;
  onTarget: (id: string) => void;
  /** opens the restore of a snapshot; without it, nothing is restored here */
  onRestore?: (snapshot: BackupSnapshot) => void;
  onFiles?: (snapshot: BackupSnapshot) => void;
  onShowRestore: (id: string) => void;
}) {
  const { t } = useTranslation();
  const [shown, setShown] = useState<{ key: string; all: boolean }>({ key: "", all: false });
  const key = target ? `${job.id}|${target.id}` : null;
  const listing = useListing(key, (refresh) => restoresApi.snapshots(job.id, target!.id, refresh));
  const host = hosts.find((item) => item.id === job.host)?.name ?? "–";
  // the panel's own copy (#32), which its carrier uploads
  const server =
    job.source_kind === "panel"
      ? `${t("backups.job.panelHost")} · ${t("backups.job.via", { server: host })}`
      : host;
  const targetServer = hosts.find((item) => item.id === target?.host)?.name ?? host;
  const list = listing.data;
  const all = shown.key === key && shown.all;
  // a snapshot whose summary restic did not keep is measured by its run
  const runOf = (snapshot: BackupSnapshot) =>
    runs.find((run) => run.job === job.id && run.snapshot_id === snapshot.id);

  let body: ReactNode;
  if (!target) {
    body = <EmptyState title={t("backups.snapshot.noTarget")} />;
  } else if (listing.error) {
    const status = listing.error instanceof ClientResponseError ? listing.error.status : 0;
    const message = errorMessage(listing.error);
    body = (
      <div className="card-pad">
        {status === 409 ? (
          <Banner tone="warn">{message}</Banner>
        ) : (
          <Banner tone="err">
            <span>{t("backups.snapshot.failed", { error: message })}</span>{" "}
            <Button size="small" onClick={() => listing.reload()}>
              {t("backups.snapshot.retry")}
            </Button>
          </Banner>
        )}
      </div>
    );
  } else if (!list) {
    body = (
      <>
        <div className="card-pad">
          <Banner tone="info">{t("backups.snapshot.loading", { host: targetServer })}</Banner>
        </div>
        <ul className="list snapshot-list" aria-hidden="true">
          {Array.from({ length: 5 }, (_, index) => (
            <li key={index} className="snapshot-row skeleton-row">
              <span className="skeleton" />
            </li>
          ))}
        </ul>
      </>
    );
  } else if (list.snapshots.length === 0) {
    body = (
      <EmptyState icon={History} title={t("backups.snapshot.empty", { target: target.name })} />
    );
  } else {
    const rows = all ? list.snapshots : list.snapshots.slice(0, firstRows);
    const older = list.snapshots.length - rows.length;
    body = (
      <>
        <ul className="list snapshot-list" aria-label={t("backups.snapshot.title")}>
          {rows.map((snapshot, index) => (
            <li key={snapshot.id} className="snapshot-row">
              <span className="snapshot-id mono" title={snapshot.id}>
                {snapshot.short_id || snapshot.id.slice(0, 8)}
              </span>
              <span className="snapshot-time">
                {formatSnapshotTime(snapshot.time, now, undefined, t)}
              </span>
              <span className="snapshot-size" title={t("backups.snapshot.sizeTitle")}>
                {formatBytes(snapshot.size_bytes || runOf(snapshot)?.total_bytes)}
              </span>
              {index === 0 ? (
                <span className="snapshot-delta">{t("backups.snapshot.newest")}</span>
              ) : (
                <AddedBytes bytes={snapshot.added_bytes || runOf(snapshot)?.bytes_added || 0} />
              )}
              {canRestore && onRestore && onFiles && (
                <span className="row-actions snapshot-actions">
                  <Button
                    size="small"
                    aria-label={t("backups.snapshot.filesName", { id: snapshot.short_id })}
                    onClick={() => onFiles(snapshot)}
                  >
                    {t("backups.snapshot.files")}
                  </Button>
                  <Button
                    size="small"
                    aria-label={t("backups.snapshot.restoreName", { id: snapshot.short_id })}
                    onClick={() => onRestore(snapshot)}
                  >
                    {t("backups.snapshot.restore")}
                  </Button>
                </span>
              )}
            </li>
          ))}
        </ul>
        {older > 0 && (
          <div className="card-pad snapshot-more">
            <Button size="small" onClick={() => setShown({ key: key!, all: true })}>
              {t("backups.snapshot.showOlder", { count: older })}
            </Button>
          </div>
        )}
      </>
    );
  }

  return (
    <Card
      title={
        <>
          {t("backups.snapshot.title")} · <span className="mono">{job.name}</span>
        </>
      }
      note={target ? t("backups.snapshot.route", { host: server, target: target.name }) : server}
      side={
        target && (
          <>
            {sources.length > 1 && (
              <SegmentedControl
                label={t("backups.snapshot.target")}
                value={target.id}
                onChange={onTarget}
                options={sources.map((source) => ({
                  value: source.target.id,
                  label: source.old
                    ? t("backups.snapshot.oldTarget", { target: source.target.name })
                    : source.target.name,
                }))}
              />
            )}
            {list && (
              <span className="muted small">
                {t("backups.snapshot.fetched", { time: formatMoment(list.fetched_at, now) })}
              </span>
            )}
            <IconButton
              bare
              aria-label={t("backups.snapshot.refresh")}
              disabled={listing.loading}
              onClick={() => listing.reload(true)}
            >
              <RefreshCw size={15} aria-hidden="true" />
            </IconButton>
          </>
        )
      }
      flush
    >
      {open && (
        <div className="card-pad">
          <Banner tone="info">
            <span>{t("backups.snapshot.restoring", { job: job.name })}</span>{" "}
            <Button size="small" onClick={() => onShowRestore(open.id)}>
              {t("backups.snapshot.show")}
            </Button>
          </Banner>
        </div>
      )}
      {body}
      <div className="snapshot-footer muted small">
        {list?.truncated && (
          <p>{t("backups.snapshot.truncated", { count: list.snapshots.length })}</p>
        )}
        <p>{t("backups.snapshot.footer")}</p>
      </div>
    </Card>
  );
}

/** What a backup added to the repository. */
function AddedBytes({ bytes }: { bytes: number }) {
  const { t } = useTranslation();
  return (
    <span className="snapshot-delta mono" title={t("backups.snapshot.addedTitle")}>
      {bytes > 0 ? t("backups.snapshot.added", { size: formatBytes(bytes) }) : "–"}
    </span>
  );
}

/** The newest restores of a job, with how each went. */
export function RecentRestores({
  restores,
  hosts,
  now,
  onShow,
}: {
  restores: BackupRestore[];
  hosts: Host[];
  now: number;
  onShow: (id: string) => void;
}) {
  const { t } = useTranslation();
  if (restores.length === 0) {
    return null;
  }
  return (
    <Card title={t("backups.snapshot.recent")} flush>
      <ul className="list" aria-label={t("backups.snapshot.recent")}>
        {restores.map((restore) => {
          const server = hosts.find((host) => host.id === restore.host)?.name;
          const when = restore.finished_at || restore.started_at || restore.created;
          return (
            <li key={restore.id} className="list-row restore-row">
              <div>
                <div className="restore-label">{restore.label}</div>
                <div className="muted small">
                  {[
                    formatMoment(when, now),
                    server,
                    restore.confirmed_label || restore.requested_label,
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </div>
              </div>
              <div className="row-actions">
                <StatusPill tone={restoreTone(restore.status)} dot={restoreOpen(restore)}>
                  {t(`backups.restore.status.${restore.status}`)}
                </StatusPill>
                <Button size="small" onClick={() => onShow(restore.id)}>
                  {t("backups.snapshot.show")}
                </Button>
              </div>
            </li>
          );
        })}
      </ul>
    </Card>
  );
}
