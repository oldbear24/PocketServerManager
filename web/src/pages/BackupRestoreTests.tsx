import { ClipboardX } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { CommandOutput } from "../components/CommandOutput";
import { useToast } from "../components/toast";
import { Button, Card, Drawer, IssueBanner, StatusPill } from "../components/ui";
import {
  checkRetryAt,
  formatCheckMoment,
  restoreTestTone,
  type BackupCheck,
} from "../lib/backupChecks";
import { jobsApi, sourceGone, type BackupJob } from "../lib/backupJobs";
import type { BackupTarget } from "../lib/backups";
import { latestTest, nextCron } from "../lib/databaseBackups";
import { formatBytes, formatDuration } from "../lib/format";
import { useMounted } from "../lib/mounted";
import { useLiveRecord } from "../lib/realtime";
import type { Alert } from "../lib/types";

type Translate = (key: string, options?: Record<string, unknown>) => string;

/** The order the card lists the tests in: a backup that could not be
 * restored first, then a test that did not complete or was skipped. */
const order: Record<string, number> = { failed: 0, error: 1, skipped: 2 };

/** A database job's line: its newest restore test, if any. */
export type TestRow = { job: BackupJob; test: BackupCheck | null };

/** The database jobs whose restore tests the card lists: those whose
 * database is there, and a dropped one's while it has a test; failed first,
 * then by name. */
export function testRows(jobs: BackupJob[], tests: BackupCheck[]): TestRow[] {
  return jobs
    .filter((job) => job.origin === "database")
    .map((job) => ({ job, test: latestTest(tests, job.id) }))
    .filter((row) => row.test !== null || !sourceGone(row.job))
    .sort(
      (a, b) =>
        (order[a.test?.status ?? ""] ?? 9) - (order[b.test?.status ?? ""] ?? 9) ||
        a.job.name.localeCompare(b.job.name),
    );
}

/** When a test happened: when it ended, started or was made. */
function testMoment(test: Pick<BackupCheck, "finished_at" | "started_at" | "created">): string {
  return test.finished_at || test.started_at || test.created;
}

/** The tables a test restored, of those in the dump: "12 / 12". */
function tablesText(test: BackupCheck): string {
  return (test.tables_expected ?? 0) > 0 || (test.tables_restored ?? 0) > 0
    ? `${test.tables_restored ?? 0} / ${test.tables_expected ?? 0}`
    : "–";
}

/** How the restore tests stand, for the integrity tile's second line: how
 * many database jobs' newest test passed, of all, and when the next test
 * comes; or that the tests are off. Null without database jobs. */
export function restoreTestsLine(
  jobs: BackupJob[],
  tests: BackupCheck[],
  schedule: string | undefined,
  now: number,
  t: Translate,
): string | null {
  const counted = jobs.filter((job) => job.origin === "database" && !sourceGone(job));
  if (counted.length === 0 || schedule === undefined) {
    return null;
  }
  if (schedule.trim() === "") {
    return t("backups.restoreTests.off");
  }
  const ok = counted.filter((job) => latestTest(tests, job.id, true)?.status === "ok").length;
  const next = nextCron(schedule, now);
  return t("backups.restoreTests.kpi", {
    ok,
    total: counted.length,
    when: next === null ? "–" : formatCheckMoment(new Date(next).toISOString(), now),
  });
}

/** The restore tests of the database jobs (#53), beside the targets: each
 * job's newest test, failed first, and when the next comes. A row opens the
 * test's details (?test=). */
export function RestoreTestsCard({
  jobs,
  tests,
  schedule,
  now,
  onShow,
}: {
  jobs: BackupJob[];
  tests: BackupCheck[];
  /** the settings' restore_test_schedule; undefined until it is loaded */
  schedule: string | undefined;
  now: number;
  onShow: (test: BackupCheck) => void;
}) {
  const { t } = useTranslation();
  const rows = testRows(jobs, tests);
  if (rows.length === 0) {
    return null;
  }
  const next = schedule ? nextCron(schedule, now) : null;
  return (
    <Card
      id="restore-tests"
      title={t("backups.restoreTests.title")}
      side={
        schedule !== undefined && (
          <span className="muted small">
            {schedule.trim() === ""
              ? t("backups.restoreTests.off")
              : t("backups.restoreTests.next", {
                  when: next === null ? "–" : formatCheckMoment(new Date(next).toISOString(), now),
                })}
          </span>
        )
      }
      flush
    >
      <p className="card-pad muted small restore-tests-note">{t("backups.restoreTests.note")}</p>
      <ul className="list restore-tests" aria-label={t("backups.restoreTests.title")}>
        {rows.map(({ job, test }) => {
          const name = test?.database_name || job.source;
          const retryAt = test ? checkRetryAt(test) : "";
          return (
            <li key={job.id} className="list-row restore-test">
              <div className="restore-test-head">
                {test ? (
                  <button type="button" className="link-button mono" onClick={() => onShow(test)}>
                    {name}
                  </button>
                ) : (
                  <span className="mono">{name}</span>
                )}
                <span className="faint small">{job.db_server_name}</span>
                <span className="restore-test-result">
                  {test ? (
                    <StatusPill tone={restoreTestTone(test.status)}>
                      {t(`backups.restoreTests.status.${test.status}`)}
                    </StatusPill>
                  ) : (
                    <span className="faint small">{t("backups.restoreTests.none")}</span>
                  )}
                </span>
              </div>
              {test && (
                <dl className="test-facts small">
                  <div>
                    <dt>{t("backups.restoreTests.column.when")}</dt>
                    <dd>{formatCheckMoment(testMoment(test), now)}</dd>
                  </div>
                  <div>
                    <dt>{t("backups.restoreTests.column.snapshot")}</dt>
                    <dd className="mono">{(test.snapshot_id ?? "").slice(0, 8) || "–"}</dd>
                  </div>
                  <div>
                    <dt>{t("backups.restoreTests.column.tables")}</dt>
                    <dd>{tablesText(test)}</dd>
                  </div>
                  <div>
                    <dt>{t("backups.restoreTests.column.took")}</dt>
                    <dd>{formatDuration((test.duration_ms || 0) / 1000)}</dd>
                  </div>
                </dl>
              )}
              {test?.error && test.status !== "ok" && (
                <div className={`restore-test-error small ${test.status}`} title={test.error}>
                  {test.error}
                </div>
              )}
              {retryAt && (
                <div className="run-retry">
                  {t("backups.restoreTests.retryAt", { time: formatCheckMoment(retryAt, now) })}
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </Card>
  );
}

/** A restore test's details (?test=<check>): what it restored, from where,
 * how it went, its error and its output; operators test again. */
export function RestoreTestDrawer({
  id,
  jobs,
  targets,
  now,
  canRun,
  onClose,
}: {
  id: string;
  jobs: BackupJob[];
  targets: BackupTarget[];
  now: number;
  canRun: boolean;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const toast = useToast();
  const mounted = useMounted();
  const { record: test } = useLiveRecord<BackupCheck>("backup_checks", id);
  const [output, setOutput] = useState(false);
  const [busy, setBusy] = useState(false);
  const job = jobs.find((item) => item.id === test?.job) ?? null;
  const target = targets.find((item) => item.id === test?.target);
  const name = test?.database_name || job?.source || "";
  const retryAt = test ? checkRetryAt(test) : "";

  const again = async () => {
    if (!job) {
      return;
    }
    setBusy(true);
    try {
      await jobsApi.restoreTest(job.id);
      toast.success(t("db.backups.testQueued"));
    } catch (err) {
      toast.error(err);
    } finally {
      if (mounted.current) {
        setBusy(false);
      }
    }
  };

  return (
    <Drawer
      title={
        name ? `${t("backups.restoreTests.title")} · ${name}` : t("backups.restoreTests.title")
      }
      onClose={onClose}
      actions={
        <>
          {canRun && job && !sourceGone(job) && (
            <Button disabled={busy} onClick={() => void again()}>
              {t("db.backups.testNow")}
            </Button>
          )}
          <Button onClick={onClose}>{t("common.close")}</Button>
        </>
      }
    >
      {!test ? (
        <p className="muted">{t("common.loading")}</p>
      ) : (
        <>
          <dl className="facts">
            <dt>{t("backups.restoreTests.column.result")}</dt>
            <dd>
              <StatusPill tone={restoreTestTone(test.status)}>
                {t(`backups.restoreTests.status.${test.status}`)}
              </StatusPill>
            </dd>
            <dt>{t("backups.restoreTests.column.database")}</dt>
            <dd className="mono">{job?.db_server_name ? `${job.db_server_name}/${name}` : name}</dd>
            <dt>{t("db.backups.job")}</dt>
            <dd>{job?.name ?? "–"}</dd>
            <dt>{t("backups.restore.facts.target")}</dt>
            <dd>{target?.name ?? "–"}</dd>
            <dt>{t("backups.restoreTests.column.snapshot")}</dt>
            <dd className="mono" title={test.snapshot_id}>
              {(test.snapshot_id ?? "").slice(0, 8) || "–"}
              {(test.dump_bytes ?? 0) > 0 && ` · ${formatBytes(test.dump_bytes)}`}
            </dd>
            <dt>{t("backups.restoreTests.column.when")}</dt>
            <dd>{formatCheckMoment(testMoment(test), now)}</dd>
            <dt>{t("backups.restoreTests.column.tables")}</dt>
            <dd>{tablesText(test)}</dd>
            <dt>{t("backups.restoreTests.column.took")}</dt>
            <dd>{formatDuration((test.duration_ms || 0) / 1000)}</dd>
          </dl>
          {test.temporary && test.status === "running" && (
            <p className="muted small">
              {t("db.backups.testState.running", { name: test.temporary })}
            </p>
          )}
          {test.error && <pre className="backup-target-error failed">{test.error}</pre>}
          {retryAt && (
            <p className="muted small">
              {t("backups.restoreTests.retryAt", { time: formatCheckMoment(retryAt, now) })}
            </p>
          )}
          {test.command && (
            <div>
              <Button size="small" aria-expanded={output} onClick={() => setOutput(!output)}>
                {t("backups.integrity.showOutput")}
              </Button>
            </div>
          )}
          {output && test.command && (
            <CommandOutput
              commandId={test.command}
              title={`${t("backups.restoreTests.title")} · ${name}`}
              onClose={() => setOutput(false)}
            />
          )}
        </>
      )}
    </Drawer>
  );
}

/** The open alerts that a restore test failed: the backup could not be
 * restored (critical). */
export function RestoreTestBanner({
  alerts,
  onShow,
}: {
  alerts: Pick<Alert, "rule" | "status">[];
  onShow: () => void;
}) {
  const { t } = useTranslation();
  const count = alerts.filter(
    (alert) => alert.rule === "backup_restore_test_failed" && alert.status !== "resolved",
  ).length;
  if (count === 0) {
    return null;
  }
  return (
    <IssueBanner
      id="restore-tests-failed"
      tone="err"
      icon={<ClipboardX size={20} aria-hidden="true" />}
      title={t("backups.restoreTests.banner", { count })}
      actions={<Button onClick={onShow}>{t("backups.restoreTests.bannerAction")}</Button>}
    />
  );
}
