import { Download, Search } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { followEvents } from "../lib/api";
import { formatTime } from "../lib/format";
import {
  compileFilter,
  levelTone,
  logFileText,
  splitLevel,
  visibleLines,
  type LogEntry,
  type LogEvent,
} from "../lib/logs";
import { Button, Card, IconButton, SegmentedControl, StatusPill } from "./ui";

/** What a browser keeps. Logs are never stored by the panel, so this is all there is. */
const keepLines = 5000;
const showLines = 1000;
const renderDelay = 100;
const retryMin = 1000;
const retryMax = 30000;

type Status = "connecting" | "live" | "ended";

function LogText({ text }: { text: string }) {
  const parts = splitLevel(text);
  if (!parts) {
    return <>{text}</>;
  }

  const tone = levelTone(parts.level);

  return (
    <>
      {parts.before}
      <span className={tone === "info" ? "log-level" : `${tone}-text`}>{parts.level}</span>
      {parts.after}
    </>
  );
}

/**
 * Follows the log stream of containers. Lines stay in the browser: filtering,
 * pausing and downloading work on what it has received.
 */
export function LogViewer({
  path,
  services = [],
  fileName,
  title,
}: {
  path: string;
  /** The services of a stack, for the filter buttons. */
  services?: string[];
  fileName: string;
  title?: string;
}) {
  const { t } = useTranslation();

  const lines = useRef<LogEntry[]>([]);
  const counter = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const [, setVersion] = useState(0);
  const [status, setStatus] = useState<Status>("connecting");
  const [problem, setProblem] = useState("");
  const [dropped, setDropped] = useState(0);
  const [service, setService] = useState("");
  const [filter, setFilter] = useState("");
  const [timestamps, setTimestamps] = useState(true);
  const [pausedAt, setPausedAt] = useState<number | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    let retry: ReturnType<typeof setTimeout> | undefined;
    let delay = retryMin;

    const schedule = () => {
      if (timer.current === null) {
        timer.current = setTimeout(() => {
          timer.current = null;
          setVersion((version) => version + 1);
        }, renderDelay);
      }
    };

    const onEvent = (event: LogEvent) => {
      if (event.type === "lines") {
        delay = retryMin;
        setStatus("live");
        for (const line of event.lines) {
          counter.current += 1;
          lines.current.push({ ...line, seq: counter.current });
        }
        if (lines.current.length > keepLines) {
          lines.current.splice(0, lines.current.length - keepLines);
        }
        schedule();
      } else if (event.type === "dropped") {
        setDropped((count) => count + event.count);
      } else if (event.error) {
        setProblem(event.error);
      }
    };

    const connect = () => {
      followEvents<LogEvent>(path, onEvent, controller.signal)
        .catch((error: unknown) => {
          if (!controller.signal.aborted) {
            setProblem(error instanceof Error ? error.message : String(error));
          }
        })
        .finally(() => {
          if (controller.signal.aborted) {
            return;
          }
          setStatus("ended");
          retry = setTimeout(() => {
            setStatus("connecting");
            setProblem("");
            connect();
          }, delay);
          delay = Math.min(delay * 2, retryMax);
        });
    };

    connect();

    return () => {
      controller.abort();
      clearTimeout(retry);
      if (timer.current !== null) {
        clearTimeout(timer.current);
        timer.current = null;
      }
    };
  }, [path]);

  const compiled = useMemo(() => compileFilter(filter), [filter]);

  // the list changes without state of its own: it is read again on every render
  const shown = visibleLines(lines.current, {
    service,
    regex: compiled.regex,
    upTo: pausedAt,
  });

  const waiting = pausedAt === null ? 0 : counter.current - pausedAt;

  const download = () => {
    const url = URL.createObjectURL(
      new Blob([logFileText(lines.current)], { type: "text/plain;charset=utf-8" }),
    );
    const link = document.createElement("a");
    link.href = url;
    link.download = fileName;
    link.click();
    URL.revokeObjectURL(url);
  };

  return (
    <Card
      title={title ?? t("logs.title")}
      note={
        <StatusPill tone={status === "live" ? "ok" : status === "connecting" ? "info" : "off"}>
          {t(`logs.status.${status}`)}
        </StatusPill>
      }
      flush
    >
      <div className="log-toolbar">
        {services.length > 1 && (
          <SegmentedControl
            label={t("logs.serviceFilter")}
            value={service}
            onChange={setService}
            options={[
              { value: "", label: t("logs.all") },
              ...services.map((name) => ({ value: name, label: name })),
            ]}
          />
        )}
        <div className="log-search">
          <Search size={16} aria-hidden="true" />
          <input
            className="input mono"
            type="search"
            aria-label={t("logs.filter")}
            aria-invalid={compiled.invalid}
            placeholder={t("logs.filter")}
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
          />
        </div>
        <Button
          aria-pressed={timestamps}
          onClick={() => setTimestamps((current) => !current)}
          size="small"
        >
          {t(timestamps ? "logs.timestampsOn" : "logs.timestampsOff")}
        </Button>
        <Button
          aria-pressed={pausedAt === null}
          onClick={() => setPausedAt((current) => (current === null ? counter.current : null))}
          size="small"
        >
          {pausedAt === null ? t("logs.followOn") : t("logs.followOff", { count: waiting })}
        </Button>
        <IconButton aria-label={t("logs.download")} onClick={download}>
          <Download size={16} aria-hidden="true" />
        </IconButton>
      </div>

      {compiled.invalid && <div className="log-note err-text">{t("logs.filterInvalid")}</div>}
      {problem && <div className="log-note err-text">{problem}</div>}
      {dropped > 0 && (
        <div className="log-note warn-text">{t("logs.dropped", { count: dropped })}</div>
      )}

      <div className="log-body" tabIndex={0} role="log" aria-label={title ?? t("logs.title")}>
        {shown.length === 0 ? (
          <div className="log-empty">{t(lines.current.length ? "logs.noMatch" : "logs.empty")}</div>
        ) : (
          shown
            .slice(-showLines)
            .reverse()
            .map((line) => (
              <div
                key={line.seq}
                className={`log-line${timestamps ? " with-time" : ""}${line.stream === "stderr" ? " stderr" : ""}`}
              >
                {timestamps && <span className="log-time">{formatTime(line.ts)}</span>}
                <span className="log-source">{line.service || line.container}</span>
                <span className="log-text">
                  <LogText text={line.text} />
                </span>
              </div>
            ))
        )}
      </div>
    </Card>
  );
}
