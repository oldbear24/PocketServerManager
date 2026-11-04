import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef } from "react";

import { pb } from "../lib/pb";

export type TerminalState =
  | { kind: "connecting" }
  | { kind: "open" }
  | { kind: "exited"; code: number }
  | { kind: "idle" }
  | { kind: "error"; message: string }
  | { kind: "closed" };

type ControlMessage = { type: string; code?: number; error?: string };

const theme = {
  background: "#0c0e12",
  foreground: "#e6e8ec",
  cursor: "#e6e8ec",
  selectionBackground: "#1a2238",
};

/**
 * A terminal in the browser. It is loaded only where it is needed: xterm is larger
 * than the rest of a page. Its WebSocket cannot carry an auth header, so it opens
 * with a ticket that is valid once and for a few seconds.
 */
export default function TerminalView({
  host,
  target,
  onState,
}: {
  host: string;
  target: { container?: string; host_shell?: boolean };
  onState: (state: TerminalState) => void;
}) {
  const element = useRef<HTMLDivElement>(null);
  const report = useRef(onState);

  useEffect(() => {
    report.current = onState;
  }, [onState]);

  const { container, host_shell: hostShell } = target;

  useEffect(() => {
    if (!element.current) {
      return;
    }

    const terminal = new Terminal({
      cursorBlink: true,
      fontFamily: "'Geist Mono Variable', ui-monospace, Menlo, Consolas, monospace",
      fontSize: 13,
      theme,
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(element.current);
    fit.fit();
    terminal.focus();

    let socket: WebSocket | null = null;
    let finished = false;
    let disposed = false;

    const finish = (state: TerminalState) => {
      if (!finished) {
        finished = true;
        report.current(state);
      }
    };

    const encoder = new TextEncoder();

    const input = terminal.onData((data) => {
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(encoder.encode(data));
      }
    });

    const resize = terminal.onResize(({ cols, rows }) => {
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: "resize", cols, rows }));
      }
    });

    const observer = new ResizeObserver(() => {
      if (element.current?.offsetWidth) {
        fit.fit();
      }
    });
    observer.observe(element.current);

    pb.send<{ ticket: string }>(`/api/pc/hosts/${host}/terminal`, {
      method: "POST",
      body: {
        container,
        host_shell: hostShell,
        cols: terminal.cols,
        rows: terminal.rows,
      },
    })
      .then(({ ticket }) => {
        if (disposed) {
          return;
        }

        const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
        socket = new WebSocket(
          `${scheme}//${window.location.host}/api/pc/terminal/socket?ticket=${ticket}`,
        );
        socket.binaryType = "arraybuffer";

        socket.onmessage = (event: MessageEvent<ArrayBuffer | string>) => {
          if (typeof event.data !== "string") {
            terminal.write(new Uint8Array(event.data));
            return;
          }

          const message = JSON.parse(event.data) as ControlMessage;
          switch (message.type) {
            case "open":
              report.current({ kind: "open" });
              break;
            case "exit":
              if (message.error) {
                finish({ kind: "error", message: message.error });
              } else {
                finish({ kind: "exited", code: message.code ?? 0 });
              }
              break;
            case "idle":
              finish({ kind: "idle" });
              break;
            case "error":
              finish({ kind: "error", message: message.error ?? "" });
              break;
          }
        };

        socket.onclose = () => finish({ kind: "closed" });
      })
      .catch((error: unknown) => {
        finish({ kind: "error", message: error instanceof Error ? error.message : String(error) });
      });

    return () => {
      disposed = true;
      observer.disconnect();
      input.dispose();
      resize.dispose();
      socket?.close();
      terminal.dispose();
    };
  }, [host, container, hostShell]);

  return <div className="terminal-view" ref={element} />;
}
