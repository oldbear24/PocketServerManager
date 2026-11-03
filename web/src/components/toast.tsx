import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { X } from "lucide-react";
import { useTranslation } from "react-i18next";

import { errorMessage } from "../lib/api";
import { IconButton } from "./ui";

type Toast = { id: number; tone: "ok" | "err" | "info"; message: string };

type ToastApi = {
  success: (message: string) => void;
  /** Shows an error from the panel, or any other message. */
  error: (error: unknown) => void;
};

const ToastContext = createContext<ToastApi | null>(null);

const lifetime = 6000;

export function ToastProvider({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextId = useRef(1);

  const remove = useCallback((id: number) => {
    setToasts((current) => current.filter((toast) => toast.id !== id));
  }, []);

  const add = useCallback(
    (tone: Toast["tone"], message: string) => {
      const id = nextId.current++;
      setToasts((current) => [...current, { id, tone, message }]);
      setTimeout(() => remove(id), lifetime);
    },
    [remove],
  );

  const value = useMemo<ToastApi>(
    () => ({
      success: (message) => add("ok", message),
      error: (error) => add("err", errorMessage(error)),
    }),
    [add],
  );

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {toasts.map((toast) => (
          <div key={toast.id} className={`toast ${toast.tone}`}>
            <span style={{ flex: 1 }}>{toast.message}</span>
            <IconButton
              bare
              aria-label={t("common.dismiss")}
              style={{ width: 24, height: 24 }}
              onClick={() => remove(toast.id)}
            >
              <X size={14} aria-hidden="true" />
            </IconButton>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastApi {
  const toast = useContext(ToastContext);
  if (!toast) {
    throw new Error("useToast needs a ToastProvider");
  }
  return toast;
}
