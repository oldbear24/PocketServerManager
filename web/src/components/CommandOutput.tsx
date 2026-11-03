import { X } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { followEvents } from "../lib/api";
import { Card, IconButton } from "./ui";

type OutputEvent =
  { type: "output"; text: string } | { type: "end"; status: string; error?: string };

/** What a running command writes, as it writes it. */
export function CommandOutput({
  commandId,
  title,
  status,
  progress,
  onClose,
}: {
  commandId: string;
  title: ReactNode;
  status?: ReactNode;
  progress?: string;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [text, setText] = useState("");
  const [failure, setFailure] = useState("");
  const body = useRef<HTMLPreElement>(null);

  useEffect(() => {
    const controller = new AbortController();

    followEvents<OutputEvent>(
      `/api/pc/commands/${commandId}/output`,
      (event) => {
        if (event.type === "output") {
          setText((current) => current + event.text);
        } else if (event.error) {
          setFailure(event.error);
        }
      },
      controller.signal,
    ).catch((error: unknown) => {
      if (!controller.signal.aborted) {
        setFailure(error instanceof Error ? error.message : String(error));
      }
    });

    return () => controller.abort();
  }, [commandId]);

  // the newest output stays in view
  useEffect(() => {
    if (body.current) {
      body.current.scrollTop = body.current.scrollHeight;
    }
  }, [text]);

  return (
    <Card
      title={title}
      note={progress}
      side={
        <>
          {status}
          <IconButton bare aria-label={t("common.dismiss")} onClick={onClose}>
            <X size={16} aria-hidden="true" />
          </IconButton>
        </>
      }
      flush
    >
      <pre className="command-output" ref={body} tabIndex={0} aria-label={t("stack.output")}>
        {text || t("stack.outputWaiting")}
      </pre>
      {failure && <p className="log-note err-text">{failure}</p>}
    </Card>
  );
}
