import { Check, Copy, X } from "lucide-react";
import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ComponentType,
  type ReactNode,
} from "react";
import { useTranslation } from "react-i18next";

export type Tone = "ok" | "warn" | "err" | "off" | "info";

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  /** critical: the solid red of what fixes a failure, as Unlock and retry */
  variant?: "default" | "primary" | "danger" | "warning" | "critical";
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

/** A figure in a tile. A status figure takes a tone, and then an icon too:
 * its word and icon say it, the colour only repeats them. */
export function Kpi({
  label,
  hint,
  value,
  note,
  tone,
  icon,
}: {
  label: ReactNode;
  hint?: ReactNode;
  value: ReactNode;
  note?: ReactNode;
  tone?: "ok" | "warn" | "err";
  icon?: ReactNode;
}) {
  const classes = ["kpi-value"];
  if (tone) {
    classes.push(tone);
  }
  if (icon) {
    classes.push("with-icon");
  }
  return (
    <div className="card kpi">
      <div className="kpi-label">
        <span>{label}</span>
        {hint && <span className="mono faint small">{hint}</span>}
      </div>
      <div className={classes.join(" ")}>
        {icon}
        {value}
      </div>
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

export function Banner({
  tone,
  children,
}: {
  tone: "err" | "warn" | "info" | "ok";
  children: ReactNode;
}) {
  return (
    <div className={`banner ${tone}`} role={tone === "err" ? "alert" : "status"}>
      {children}
    </div>
  );
}

/** A problem to act on, as the design's failed-backup banner: an icon, a
 * title, restic's own line in mono, what else is known, and what fixes it.
 * A labelled section, not an alert: a live region would read it out again
 * at every realtime update. */
export function IssueBanner({
  id,
  tone,
  icon,
  title,
  detail,
  meta,
  actions,
}: {
  /** names the section by its title */
  id: string;
  tone: "err" | "warn" | "info";
  icon: ReactNode;
  title: ReactNode;
  detail?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
}) {
  const titleId = `${id}-title`;
  return (
    <section className={`issue-banner ${tone}`} aria-labelledby={titleId}>
      <span className="issue-banner-icon" aria-hidden="true">
        {icon}
      </span>
      <div className="issue-banner-text">
        <h2 id={titleId} className="issue-banner-title">
          {title}
        </h2>
        {detail && <span className="issue-banner-detail mono">{detail}</span>}
        {meta && <div className="issue-banner-meta">{meta}</div>}
      </div>
      {actions && <div className="issue-banner-actions">{actions}</div>}
    </section>
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
  confirmDisabled,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: ReactNode;
  children?: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  /** Disables the confirm button, as while its request is on its way. */
  confirmDisabled?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useTranslation();
  const dialog = useRef<HTMLDialogElement>(null);
  const titleId = useId();

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
    <dialog ref={dialog} className="dialog" aria-labelledby={titleId} onCancel={onCancel}>
      <h2 id={titleId}>{title}</h2>
      {children}
      <div className="dialog-actions">
        <Button onClick={onCancel}>{t("common.cancel")}</Button>
        <Button
          variant={danger ? "danger" : "primary"}
          disabled={confirmDisabled}
          onClick={onConfirm}
        >
          {confirmLabel}
        </Button>
      </div>
    </dialog>
  );
}

/** A side panel on the native dialog element, which traps focus, and gives
 * it back to what opened it. */
export function Drawer({
  title,
  onClose,
  children,
  actions,
}: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  actions: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  // a layout effect, so it closes before the dialog leaves the page and the
  // focus can go back
  useLayoutEffect(() => {
    const element = ref.current;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (element && !element.open) {
      element.showModal?.();
    }
    return () => {
      element?.close?.();
      opener?.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      className="dialog drawer"
      aria-labelledby={titleId}
      onCancel={(event) => {
        // Escape in a dialog inside it closes only that one
        if (event.target === event.currentTarget) {
          onClose();
        }
      }}
    >
      <h2 id={titleId}>{title}</h2>
      <div className="drawer-body">{children}</div>
      <div className="dialog-actions">{actions}</div>
    </dialog>
  );
}

export type Choice = { value: string; title: string; note: string; disabled?: boolean };

/** A row of cards to pick one from, as radio buttons: the chosen one takes
 * the focus, and the arrow keys move the choice. */
export function ChoiceGroup({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string;
  options: Choice[];
  onChange: (value: string) => void;
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const enabled = options.flatMap((option, index) => (option.disabled ? [] : [index]));
  const move = (from: number, step: number) => {
    const position = enabled.indexOf(from);
    const next = enabled[(position + step + enabled.length) % enabled.length];
    onChange(options[next].value);
    refs.current[next]?.focus();
  };
  return (
    <div
      className={`choice-grid${options.length > 2 ? " three" : ""}`}
      role="radiogroup"
      aria-label={label}
    >
      {options.map((option, index) => {
        const checked = option.value === value;
        return (
          <button
            key={option.value}
            ref={(element) => {
              refs.current[index] = element;
            }}
            type="button"
            role="radio"
            aria-checked={checked}
            tabIndex={checked ? 0 : -1}
            disabled={option.disabled}
            className={`choice${checked ? " active" : ""}`}
            onClick={() => onChange(option.value)}
            onKeyDown={(event) => {
              if (event.key === "ArrowRight" || event.key === "ArrowDown") {
                event.preventDefault();
                move(index, 1);
              } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
                event.preventDefault();
                move(index, -1);
              }
            }}
          >
            <span className="choice-title">{option.title}</span>
            <span className="choice-note">{option.note}</span>
          </button>
        );
      })}
    </div>
  );
}
