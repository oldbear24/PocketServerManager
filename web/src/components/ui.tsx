import { Check, Copy, X } from "lucide-react";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ComponentType,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";

export type Tone = "ok" | "warn" | "err" | "off" | "info";

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "primary" | "danger";
  size?: "default" | "small";
};

export function Button({
  variant = "default",
  size = "default",
  className,
  ...props
}: ButtonProps) {
  const classes = ["button"];
  if (variant !== "default") {
    classes.push(variant);
  }
  if (size === "small") {
    classes.push("small");
  }
  if (className) {
    classes.push(className);
  }
  return <button type="button" className={classes.join(" ")} {...props} />;
}

type IconButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  /** Icon buttons have no visible text, so the label is required. */
  "aria-label": string;
  bare?: boolean;
};

export function IconButton({ bare, className, ...props }: IconButtonProps) {
  return (
    <button
      type="button"
      title={props["aria-label"]}
      className={`button icon-button${bare ? " bare" : ""}${className ? ` ${className}` : ""}`}
      {...props}
    />
  );
}

/** A status is never colour-only: every pill comes with a text label. */
export function StatusPill({
  tone,
  dot = true,
  children,
}: {
  tone: Tone;
  dot?: boolean;
  children: ReactNode;
}) {
  return (
    <span className={`pill ${tone}`}>
      {dot && <span className="dot" aria-hidden="true" />}
      {children}
    </span>
  );
}

export function Card({
  title,
  note,
  side,
  flush,
  children,
  id,
}: {
  title?: ReactNode;
  note?: ReactNode;
  side?: ReactNode;
  flush?: boolean;
  children: ReactNode;
  id?: string;
}) {
  return (
    <section className="card" id={id}>
      {(title || side) && (
        <div className="card-head">
          {title && <h2>{title}</h2>}
          {note && <span className="card-head-note">{note}</span>}
          {side && <div className="card-head-side">{side}</div>}
        </div>
      )}
      <div className={`card-body${flush ? " flush" : ""}`}>{children}</div>
    </section>
  );
}

export function Kpi({
  label,
  hint,
  value,
  note,
}: {
  label: ReactNode;
  hint?: ReactNode;
  value: ReactNode;
  note?: ReactNode;
}) {
  return (
    <div className="card kpi">
      <div className="kpi-label">
        <span>{label}</span>
        {hint && <span className="mono faint small">{hint}</span>}
      </div>
      <div className="kpi-value">{value}</div>
      {note && <div className="kpi-note">{note}</div>}
    </div>
  );
}

export function Table({
  label,
  compact,
  children,
}: {
  label: string;
  compact?: boolean;
  children: ReactNode;
}) {
  return (
    // the container scrolls, so it must be reachable with a keyboard
    <div className="table-scroll" tabIndex={0} role="region" aria-label={label}>
      <table className={`table${compact ? " compact" : ""}`}>{children}</table>
    </div>
  );
}

export function ProgressBar({
  value,
  label,
  warnAt = 80,
  errAt = 90,
}: {
  value: number;
  label: string;
  warnAt?: number;
  errAt?: number;
}) {
  const level = value >= errAt ? " err" : value >= warnAt ? " warn" : "";
  return (
    <div
      className={`progress${level}`}
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(value)}
    >
      <span style={{ width: `${value}%` }} />
    </div>
  );
}

export type TabItem = { id: string; label: ReactNode; count?: ReactNode };

export function Tabs({
  items,
  active,
  onChange,
  label,
}: {
  items: TabItem[];
  active: string;
  onChange: (id: string) => void;
  label: string;
}) {
  const refs = useRef<Record<string, HTMLButtonElement | null>>({});

  const onKeyDown = (event: React.KeyboardEvent, index: number) => {
    const step = event.key === "ArrowRight" ? 1 : event.key === "ArrowLeft" ? -1 : 0;
    if (!step) {
      return;
    }
    event.preventDefault();
    const next = items[(index + step + items.length) % items.length];
    onChange(next.id);
    refs.current[next.id]?.focus();
  };

  return (
    <div className="tabs" role="tablist" aria-label={label}>
      {items.map((item, index) => (
        <button
          key={item.id}
          ref={(element) => {
            refs.current[item.id] = element;
          }}
          type="button"
          role="tab"
          aria-selected={item.id === active}
          tabIndex={item.id === active ? 0 : -1}
          className={`tab${item.id === active ? " active" : ""}`}
          onClick={() => onChange(item.id)}
          onKeyDown={(event) => onKeyDown(event, index)}
        >
          {item.label}
          {item.count !== undefined && <span className="tab-count">{item.count}</span>}
        </button>
      ))}
    </div>
  );
}

export function SegmentedControl<T extends string>({
  options,
  value,
  onChange,
  label,
}: {
  options: { value: T; label: ReactNode }[];
  value: T;
  onChange: (value: T) => void;
  label: string;
}) {
  return (
    <div className="segmented" role="group" aria-label={label}>
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          aria-pressed={option.value === value}
          onClick={() => onChange(option.value)}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

export function CodeBlock({ code, label }: { code: string; label: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(timer.current), []);

  const copy = useCallback(async () => {
    await navigator.clipboard.writeText(code);
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 2000);
  }, [code]);

  return (
    <div className="code-block">
      <pre tabIndex={0} role="region" aria-label={label}>
        <code>{code}</code>
      </pre>
      <Button size="small" className="copy" onClick={() => void copy()}>
        {copied ? <Check size={14} aria-hidden="true" /> : <Copy size={14} aria-hidden="true" />}
        {copied ? t("common.copied") : t("common.copy")}
      </Button>
    </div>
  );
}

export type TimelineState = "done" | "active" | "pending" | "failed";

export type TimelineItem = {
  id: string;
  state: TimelineState;
  title: ReactNode;
  note?: ReactNode;
  side?: ReactNode;
};

export function Timeline({ items, label }: { items: TimelineItem[]; label: string }) {
  return (
    <ol className="timeline" aria-label={label}>
      {items.map((item) => (
        <li key={item.id} className={item.state}>
          <span className="timeline-mark" aria-hidden="true">
            {item.state === "done" && <Check size={14} />}
            {item.state === "failed" && <X size={14} />}
            {item.state === "active" && <span className="status-dot info" />}
          </span>
          <div className="timeline-text">
            <div className="timeline-title">{item.title}</div>
            {item.note && <div className="timeline-note">{item.note}</div>}
          </div>
          {item.side && <div className="timeline-side">{item.side}</div>}
        </li>
      ))}
    </ol>
  );
}

export function EmptyState({
  icon: Icon,
  title,
  children,
  action,
}: {
  icon?: ComponentType<{ size?: number }>;
  title: ReactNode;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      {Icon && <Icon size={24} />}
      <h3>{title}</h3>
      {children && <p>{children}</p>}
      {action}
    </div>
  );
}

export function Banner({ tone, children }: { tone: "err" | "warn" | "info"; children: ReactNode }) {
  return (
    <div className={`banner ${tone}`} role={tone === "err" ? "alert" : "status"}>
      {children}
    </div>
  );
}

export function Field({
  label,
  htmlFor,
  error,
  children,
}: {
  label: string;
  htmlFor: string;
  error?: string;
  children: ReactNode;
}) {
  return (
    <div className="field">
      <label htmlFor={htmlFor}>{label}</label>
      {children}
      {error && <div className="field-error">{error}</div>}
    </div>
  );
}

/** A modal that traps focus by using the native dialog element. */
export function ConfirmDialog({
  open,
  title,
  children,
  confirmLabel,
  danger,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: ReactNode;
  children?: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const dialog = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const element = dialog.current;
    if (!element) {
      return;
    }
    if (open && !element.open) {
      element.showModal?.();
    }
    if (!open && element.open) {
      element.close?.();
    }
  }, [open]);

  return (
    <dialog ref={dialog} className="dialog" onCancel={onCancel}>
      <h2>{title}</h2>
      {children}
      <div className="dialog-actions">
        <Button onClick={onCancel}>{t("common.cancel")}</Button>
        <Button variant={danger ? "danger" : "primary"} onClick={onConfirm}>
          {confirmLabel}
        </Button>
      </div>
    </dialog>
  );
}
