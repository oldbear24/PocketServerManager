import { Download, RotateCcw, ShieldAlert, ShieldCheck } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { CommandStatusPill } from "../components/status";
import { useToast } from "../components/toast";
import {
  Button,
  Card,
  ConfirmDialog,
  EmptyState,
  SegmentedControl,
  StatusPill,
  Tabs,
  type Tone,
} from "../components/ui";
import { api, download } from "../lib/api";
import {
  auditFilter,
  emptyFilters,
  initials,
  lineDiff,
  withContext,
  type AuditFilters,
} from "../lib/audit";
import { hasRole, useAuth } from "../lib/auth";
import { formatDateTime, formatDay, formatTime, parseDate } from "../lib/format";
import { pb } from "../lib/pb";
import { useLiveList } from "../lib/realtime";
import type { AuditChange, AuditEntry, Command, Host, VerifyResult } from "../lib/types";

const pageSize = 50;

type TabId = "changes" | "commands" | "security";

function useFilters() {
  const [search, setSearch] = useSearchParams();

  const filters: AuditFilters = useMemo(
    () => ({
      q: search.get("q") ?? emptyFilters.q,
      window: (search.get("window") as AuditFilters["window"]) ?? emptyFilters.window,
      actor: search.get("actor") ?? "",
      collection: search.get("collection") ?? "",
      host: search.get("host") ?? "",
      action: search.get("action") ?? "",
      record: search.get("record") ?? "",
    }),
    [search],
  );

  // filters live in the URL, so that a view can be shared
  const set = useCallback(
    (key: keyof AuditFilters, value: string) => {
      setSearch(
        (current) => {
          const next = new URLSearchParams(current);
          if (value && value !== emptyFilters[key]) {
            next.set(key, value);
          } else {
            next.delete(key);
          }
          return next;
        },
        { replace: true },
      );
    },
    [setSearch],
  );

  return { filters, set, query: search.toString() };
}

export function AuditPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const navigate = useNavigate();
  const params = useParams();
  const { filters, set, query } = useFilters();

  const isAdmin = hasRole(user, "admin");
  const tab = (
    ["commands", "security"].includes(params.tab ?? "") ? params.tab : "changes"
  ) as TabId;

  const [verify, setVerify] = useState<(VerifyResult & { at: number }) | null>(null);
  const [verifying, setVerifying] = useState(false);

  const now = useRef(Date.now());
  useEffect(() => {
    now.current = Date.now();
  }, [filters.window]);

  const hosts = useLiveList<Host>("hosts", { sort: "name" });

  const counts = {
    changes: useLiveList<AuditEntry>("audit_log", {
      filter: auditFilter("change", filters, now.current),
      perPage: 1,
    }).total,
    security: useLiveList<AuditEntry>("audit_log", {
      filter: auditFilter("security", filters, now.current),
      perPage: 1,
      enabled: isAdmin,
    }).total,
    commands: useLiveList<Command>("commands", { perPage: 1 }).total,
  };

  const runVerify = async () => {
    setVerifying(true);
    try {
      setVerify({ ...(await api.verifyAudit()), at: Date.now() });
    } catch (err) {
      toast.error(err);
    } finally {
      setVerifying(false);
    }
  };

  const exportLog = async () => {
    try {
      const filter = encodeURIComponent(
        auditFilter(tab === "security" ? "security" : "change", filters, now.current),
      );
      await download(`/api/pc/audit/export?filter=${filter}`, "audit-log.jsonl");
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <PageHead
        crumbs={`${t("nav.system")} / ${t("nav.audit")}`}
        title={t("audit.title")}
        description={t("audit.description")}
        actions={
          isAdmin && (
            <>
              {verify &&
                (verify.ok ? (
                  <StatusPill tone="ok" dot={false}>
                    <ShieldCheck size={14} aria-hidden="true" />
                    {t("audit.chainOk", { time: formatTime(new Date(verify.at).toISOString()) })}
                  </StatusPill>
                ) : (
                  <StatusPill tone="err" dot={false}>
                    <ShieldAlert size={14} aria-hidden="true" />
                    {t("audit.chainBroken", { seq: verify.bad_seq })}
                  </StatusPill>
                ))}
              <Button disabled={verifying} onClick={() => void runVerify()}>
                {t("audit.verify")}
              </Button>
              <Button variant="primary" onClick={() => void exportLog()}>
                <Download size={16} aria-hidden="true" />
                {t("audit.export")}
              </Button>
            </>
          )
        }
      />

      <Tabs
        label={t("nav.audit")}
        active={tab}
        onChange={(next) =>
          navigate(`/audit/${next === "changes" ? "" : next}${query ? `?${query}` : ""}`)
        }
        items={[
          { id: "changes", label: t("audit.tab.changes"), count: counts.changes },
          { id: "commands", label: t("audit.tab.commands"), count: counts.commands },
          ...(isAdmin
            ? [{ id: "security", label: t("audit.tab.security"), count: counts.security }]
            : []),
        ]}
      />

      {tab === "commands" ? (
        <CommandsTab hosts={hosts.items} />
      ) : (
        <EntriesTab
          category={tab === "security" ? "security" : "change"}
          filters={filters}
          set={set}
          since={now.current}
          hosts={hosts.items}
        />
      )}
    </>
  );
}

function Filters({
  filters,
  set,
  hosts,
  showType,
}: {
  filters: AuditFilters;
  set: (key: keyof AuditFilters, value: string) => void;
  hosts: Host[];
  showType: boolean;
}) {
  const { t } = useTranslation();

  return (
    <div className="filters">
      <div className="search">
        <label htmlFor="audit-search" className="visually-hidden">
          {t("audit.search")}
        </label>
        <input
          id="audit-search"
          className="input"
          placeholder={t("audit.search")}
          value={filters.q}
          onChange={(event) => set("q", event.target.value)}
        />
      </div>
      <SegmentedControl
        label={t("audit.window")}
        value={filters.window}
        onChange={(value) => set("window", value)}
        options={[
          { value: "24h", label: t("audit.window24h") },
          { value: "7d", label: t("audit.window7d") },
          { value: "30d", label: t("audit.window30d") },
          { value: "all", label: t("audit.windowAll") },
        ]}
      />
      {showType && (
        <select
          className="select"
          aria-label={t("audit.type")}
          value={filters.collection}
          onChange={(event) => set("collection", event.target.value)}
        >
          <option value="">{t("audit.typeAll")}</option>
          {["hosts", "stacks", "users", "settings", "_params", "_collections"].map((name) => (
            <option key={name} value={name}>
              {t(`audit.collection.${name}`, name)}
            </option>
          ))}
        </select>
      )}
      <select
        className="select"
        aria-label={t("host.server")}
        value={filters.host}
        onChange={(event) => set("host", event.target.value)}
      >
        <option value="">{t("audit.serverAll")}</option>
        {hosts.map((host) => (
          <option key={host.id} value={host.id}>
            {host.name}
          </option>
        ))}
      </select>
      <select
        className="select"
        aria-label={t("audit.action")}
        value={filters.action}
        onChange={(event) => set("action", event.target.value)}
      >
        <option value="">{t("audit.actionAll")}</option>
        {(showType
          ? ["create", "update", "delete"]
          : [
              "sign_in",
              "sign_in_failed",
              "enrollment",
              "enrollment_failed",
              "enrollment_token",
              "cert_rejected",
              "cert_revoked",
              "terminal_session",
              "integrity_broken",
            ]
        ).map((action) => (
          <option key={action} value={action}>
            {t(`audit.actionName.${action}`, action)}
          </option>
        ))}
      </select>
    </div>
  );
}

function actionTone(action: string): Tone {
  switch (action) {
    case "create":
    case "sign_in":
    case "enrollment":
      return "ok";
    case "delete":
    case "sign_in_failed":
    case "enrollment_failed":
    case "cert_rejected":
    case "integrity_broken":
      return "err";
    case "update":
    case "revert":
      return "info";
    default:
      return "off";
  }
}

function sourceLabel(source: string, t: (key: string, fallback: string) => string) {
  return t(`audit.source.${source}`, source);
}

/** Loads more as soon as the end of the list comes into view. */
function LoadMore({ onVisible, label }: { onVisible: () => void; label: string }) {
  const element = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const target = element.current;
    if (!target || typeof IntersectionObserver === "undefined") {
      return;
    }
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) {
        onVisible();
      }
    });
    observer.observe(target);
    return () => observer.disconnect();
  }, [onVisible]);

  return (
    <div ref={element} className="card-body muted small">
      {label}
    </div>
  );
}

function EntriesTab({
  category,
  filters,
  set,
  since,
  hosts,
}: {
  category: "change" | "security";
  filters: AuditFilters;
  set: (key: keyof AuditFilters, value: string) => void;
  since: number;
  hosts: Host[];
}) {
  const { t } = useTranslation();
  const [limit, setLimit] = useState(pageSize);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const entries = useLiveList<AuditEntry>("audit_log", {
    filter: auditFilter(category, filters, since),
    sort: "-seq",
    perPage: limit,
  });

  const selected =
    entries.items.find((entry) => entry.id === selectedId) ?? entries.items[0] ?? null;

  const grouped = useMemo(() => {
    const days: { day: string; entries: AuditEntry[] }[] = [];
    for (const entry of entries.items) {
      const day = formatDay(entry.at);
      const last = days[days.length - 1];
      if (last?.day === day) {
        last.entries.push(entry);
      } else {
        days.push({ day, entries: [entry] });
      }
    }
    return days;
  }, [entries.items]);

  const loadMore = useCallback(() => {
    setLimit((current) => current + pageSize);
  }, []);

  return (
    <>
      <Filters filters={filters} set={set} hosts={hosts} showType={category === "change"} />
      {filters.record && (
        <div>
          <Button size="small" onClick={() => set("record", "")}>
            {t("audit.clearObject")}
          </Button>
        </div>
      )}

      <div className="columns">
        <div className="column-wide">
          <section
            className="card"
            aria-label={t(`audit.tab.${category === "change" ? "changes" : "security"}`)}
          >
            {entries.items.length === 0 && !entries.loading && (
              <EmptyState title={t("audit.noEntries")} />
            )}
            {grouped.map((group) => (
              <div key={group.day}>
                <div className="audit-day">{group.day}</div>
                {group.entries.map((entry) => (
                  <EntryRow
                    key={entry.id}
                    entry={entry}
                    hosts={hosts}
                    selected={selected?.id === entry.id}
                    onSelect={() => setSelectedId(entry.id)}
                  />
                ))}
              </div>
            ))}
            {entries.items.length > 0 && (
              <LoadMore
                label={t("audit.shown", { shown: entries.items.length, total: entries.total })}
                onVisible={entries.items.length < entries.total ? loadMore : noop}
              />
            )}
          </section>
        </div>
        <div className="column-side" style={{ flexBasis: 420 }}>
          {selected && (
            <EntryDetail entry={selected} hosts={hosts} onHistory={(id) => set("record", id)} />
          )}
        </div>
      </div>
    </>
  );
}

const noop = () => undefined;

function summary(entry: AuditEntry): string {
  if (entry.changes) {
    const fields = Object.keys(entry.changes);
    return fields.slice(0, 4).join(", ") + (fields.length > 4 ? "…" : "");
  }
  return "";
}

function EntryRow({
  entry,
  hosts,
  selected,
  onSelect,
}: {
  entry: AuditEntry;
  hosts: Host[];
  selected: boolean;
  onSelect: () => void;
}) {
  const { t } = useTranslation();
  const hostName = hosts.find((host) => host.id === entry.host)?.name;
  const actor = entry.actor_label || t(`audit.actorKind.${entry.actor_kind}`);

  return (
    <button
      type="button"
      className={`audit-entry${selected ? " selected" : ""}`}
      aria-pressed={selected}
      onClick={onSelect}
    >
      <span className="mono muted small">{formatTime(entry.at)}</span>
      <span
        className={`avatar ${entry.actor_kind === "user" ? "" : entry.actor_kind}`}
        aria-hidden="true"
      >
        {initials(actor)}
      </span>
      <span style={{ minWidth: 0 }}>
        <span style={{ display: "block" }}>
          <strong style={{ fontWeight: 500 }}>{actor}</strong>{" "}
          {t(`audit.verb.${entry.action}`, entry.action)}{" "}
          {entry.collection && `${t(`audit.collection.${entry.collection}`, entry.collection)} `}
          {entry.record_label && (
            <span className="mono" style={{ color: "var(--accent)" }}>
              {entry.record_label}
            </span>
          )}
        </span>
        <span
          className="cell-sub"
          style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}
        >
          {summary(entry) && <span>{summary(entry)}</span>}
          <StatusPill tone="off" dot={false}>
            {sourceLabel(entry.source, t)}
          </StatusPill>
          {hostName && <span className="mono">{hostName}</span>}
        </span>
      </span>
      <StatusPill tone={actionTone(entry.action)} dot={false}>
        {t(`audit.actionName.${entry.action}`, entry.action)}
      </StatusPill>
    </button>
  );
}

function ChangeView({ field, change }: { field: string; change: AuditChange }) {
  const { t } = useTranslation();

  if (change === "changed") {
    return (
      <div className="diff-field">
        <div className="muted small">{field}</div>
        <span className="muted">{t("audit.secretChanged")}</span>
      </div>
    );
  }

  const format = (value: unknown) =>
    value === undefined || value === null || value === ""
      ? "–"
      : typeof value === "string"
        ? value
        : JSON.stringify(value);

  const from = format(change.from);
  const to = format(change.to);

  // long text reads better as a line diff: compose files, env files
  if (from.includes("\n") || to.includes("\n")) {
    return (
      <div className="diff-field">
        <div className="muted small" style={{ marginBottom: 6 }}>
          {field}
        </div>
        <div className="diff">
          {withContext(lineDiff(change.from === undefined ? "" : from, to)).map((line, index) => (
            <div key={index} className={`diff-line ${line.kind === "same" ? "" : line.kind}`}>
              <span aria-hidden="true">
                {line.kind === "add" ? "+" : line.kind === "del" ? "-" : " "}
              </span>
              <span>
                {line.kind !== "same" && (
                  <span className="visually-hidden">
                    {line.kind === "add" ? t("audit.added") : t("audit.removed")}{" "}
                  </span>
                )}
                {line.text}
              </span>
            </div>
          ))}
        </div>
      </div>
    );
  }

  return (
    <div className="diff-field">
      <div className="muted small">{field}</div>
      <div className="mono small">
        {change.from !== undefined && (
          <>
            <span className="err-text">{from}</span>
            {" → "}
          </>
        )}
        <span className="ok-text">{to}</span>
      </div>
    </div>
  );
}

function EntryDetail({
  entry,
  hosts,
  onHistory,
}: {
  entry: AuditEntry;
  hosts: Host[];
  onHistory: (recordId: string) => void;
}) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const [confirming, setConfirming] = useState(false);

  const hostName = hosts.find((host) => host.id === entry.host)?.name;
  const actor = entry.actor_label || t(`audit.actorKind.${entry.actor_kind}`);

  const canRevert =
    hasRole(user, "admin") &&
    entry.category === "change" &&
    ["create", "update", "delete"].includes(entry.action) &&
    !entry.collection.startsWith("_");

  const revert = async () => {
    setConfirming(false);
    try {
      await api.revertAudit(entry.id);
      toast.success(t("audit.reverted"));
    } catch (err) {
      toast.error(err);
    }
  };

  const changes = Object.entries(entry.changes ?? {});
  const snapshot = Object.entries(entry.snapshot ?? {});

  return (
    <Card
      title={`${t(`audit.collection.${entry.collection}`, entry.collection || t("audit.event"))} ${entry.record_label}`}
      side={
        <StatusPill tone={actionTone(entry.action)} dot={false}>
          {t(`audit.actionName.${entry.action}`, entry.action)}
        </StatusPill>
      }
    >
      <dl className="facts">
        <dt>{t("audit.when")}</dt>
        <dd>{formatDateTime(entry.at)}</dd>
        <dt>{t("audit.who")}</dt>
        <dd>
          {actor} · {t(`audit.actorKind.${entry.actor_kind}`)}
        </dd>
        <dt>{t("audit.from")}</dt>
        <dd>
          {[sourceLabel(entry.source, t), entry.ip, entry.user_agent].filter(Boolean).join(" · ")}
        </dd>
        {hostName && (
          <>
            <dt>{t("host.server")}</dt>
            <dd>
              <Link to={`/servers/${entry.host}`}>{hostName}</Link>
            </dd>
          </>
        )}
        {entry.command && (
          <>
            <dt>{t("audit.linkedCommand")}</dt>
            <dd className="mono">{entry.command}</dd>
          </>
        )}
        <dt>{t("audit.entry")}</dt>
        <dd className="mono small">
          #{entry.seq} · sha256 {entry.hash.slice(0, 8)}…{entry.hash.slice(-4)}
        </dd>
      </dl>

      {changes.length > 0 && (
        <>
          <div className="nav-heading" style={{ padding: "16px 0 6px" }}>
            {t("audit.diff")}
          </div>
          <div className="diff" style={{ fontFamily: "inherit", fontSize: "inherit" }}>
            {changes.map(([field, change]) => (
              <ChangeView key={field} field={field} change={change} />
            ))}
          </div>
        </>
      )}

      {snapshot.length > 0 && (
        <>
          <div className="nav-heading" style={{ padding: "16px 0 6px" }}>
            {entry.action === "delete" ? t("audit.lastState") : t("audit.details")}
          </div>
          <div className="diff" style={{ fontFamily: "inherit", fontSize: "inherit" }}>
            {snapshot.map(([field, value]) => (
              <div key={field} className="diff-field">
                <div className="muted small">{field}</div>
                <div className="mono small" style={{ overflowWrap: "anywhere" }}>
                  {typeof value === "string" ? value : JSON.stringify(value)}
                </div>
              </div>
            ))}
          </div>
        </>
      )}

      {entry.note && (
        <p className="muted" style={{ marginTop: 12 }}>
          {t("audit.note")}: <span style={{ color: "var(--text)" }}>{entry.note}</span>
        </p>
      )}

      <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 16 }}>
        {canRevert && (
          <Button onClick={() => setConfirming(true)}>
            <RotateCcw size={16} aria-hidden="true" />
            {t("audit.revert")}
          </Button>
        )}
        {entry.record_id && (
          <Button onClick={() => onHistory(entry.record_id)}>{t("audit.history")}</Button>
        )}
      </div>

      {canRevert && (
        <p className="muted small" style={{ marginTop: 10 }}>
          {t("audit.revertNote")}
        </p>
      )}

      <ConfirmDialog
        open={confirming}
        title={t("audit.revertConfirm", { object: entry.record_label || entry.record_id })}
        confirmLabel={t("audit.revert")}
        onConfirm={() => void revert()}
        onCancel={() => setConfirming(false)}
      >
        <p className="muted">{t("audit.revertNote")}</p>
      </ConfirmDialog>
    </Card>
  );
}

function CommandsTab({ hosts }: { hosts: Host[] }) {
  const { t } = useTranslation();
  const { user } = useAuth();
  const toast = useToast();
  const [search, setSearch] = useSearchParams();
  const [limit, setLimit] = useState(pageSize);

  const status = search.get("status") ?? "";
  const host = search.get("host") ?? "";
  const type = search.get("type") ?? "";
  const requester = search.get("requester") ?? "";

  const setFilter = (key: string, value: string) => {
    setSearch(
      (current) => {
        const next = new URLSearchParams(current);
        if (value) {
          next.set(key, value);
        } else {
          next.delete(key);
        }
        return next;
      },
      { replace: true },
    );
  };

  const filter = [
    status && pb.filter("status = {:status}", { status }),
    host && pb.filter("host = {:host}", { host }),
    type && pb.filter("type ~ {:type}", { type }),
    requester && pb.filter("requested_label ~ {:requester}", { requester }),
  ]
    .filter(Boolean)
    .join(" && ");

  const commands = useLiveList<Command>("commands", {
    filter: filter || undefined,
    sort: "-created",
    perPage: limit,
  });

  const loadMore = useCallback(() => setLimit((current) => current + pageSize), []);

  const canCancel = hasRole(user, "operator");

  const cancel = async (id: string) => {
    try {
      await api.cancelCommand(id);
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <div className="filters">
        <div className="search">
          <label htmlFor="command-type" className="visually-hidden">
            {t("audit.commandType")}
          </label>
          <input
            id="command-type"
            className="input"
            placeholder={t("audit.commandType")}
            value={type}
            onChange={(event) => setFilter("type", event.target.value)}
          />
        </div>
        <div className="search">
          <label htmlFor="command-requester" className="visually-hidden">
            {t("audit.requester")}
          </label>
          <input
            id="command-requester"
            className="input"
            placeholder={t("audit.requester")}
            value={requester}
            onChange={(event) => setFilter("requester", event.target.value)}
          />
        </div>
        <select
          className="select"
          aria-label={t("host.state")}
          value={status}
          onChange={(event) => setFilter("status", event.target.value)}
        >
          <option value="">{t("audit.statusAll")}</option>
          {["queued", "dispatched", "running", "succeeded", "failed", "timed_out", "cancelled"].map(
            (name) => (
              <option key={name} value={name}>
                {t(`command.status.${name}`)}
              </option>
            ),
          )}
        </select>
        <select
          className="select"
          aria-label={t("host.server")}
          value={host}
          onChange={(event) => setFilter("host", event.target.value)}
        >
          <option value="">{t("audit.serverAll")}</option>
          {hosts.map((item) => (
            <option key={item.id} value={item.id}>
              {item.name}
            </option>
          ))}
        </select>
      </div>

      <section className="card" aria-label={t("audit.tab.commands")}>
        {commands.items.length === 0 && !commands.loading && (
          <EmptyState title={t("audit.noCommands")} />
        )}
        <ul className="list">
          {commands.items.map((command) => {
            const open = ["queued", "dispatched", "running"].includes(command.status);
            const created = parseDate(command.created);
            const finished = parseDate(command.finished_at);
            return (
              <li key={command.id}>
                <div className="list-row">
                  <div>
                    <div className="mono">{command.type}</div>
                    <div className="cell-sub">
                      {[
                        command.label,
                        command.host_name,
                        formatDateTime(command.created),
                        command.requested_label,
                        finished && created
                          ? `${((finished.getTime() - created.getTime()) / 1000).toFixed(1)} s`
                          : "",
                        command.attempts > 1
                          ? t("audit.attempts", { count: command.attempts })
                          : "",
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </div>
                    {command.progress?.message && open && (
                      <div className="cell-sub">{command.progress.message}</div>
                    )}
                    {command.error && <div className="err-text small">{command.error}</div>}
                  </div>
                  <div style={{ display: "flex", gap: 8, alignItems: "center", flex: "none" }}>
                    {open && canCancel && (
                      <Button size="small" onClick={() => void cancel(command.id)}>
                        {t("common.cancel")}
                      </Button>
                    )}
                    <CommandStatusPill status={command.status} />
                  </div>
                </div>
              </li>
            );
          })}
        </ul>
        {commands.items.length > 0 && (
          <LoadMore
            label={t("audit.shown", { shown: commands.items.length, total: commands.total })}
            onVisible={commands.items.length < commands.total ? loadMore : noop}
          />
        )}
      </section>
    </>
  );
}
