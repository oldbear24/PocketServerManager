import { Boxes } from "lucide-react";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Navigate, useLocation, useNavigate, useSearchParams } from "react-router-dom";

import { Banner, Button, Field } from "../components/ui";
import { api, errorMessage } from "../lib/api";
import { useAuth } from "../lib/auth";

function AuthCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="auth-page">
      <main className="card auth-card">
        <div className="brand" style={{ padding: 0 }}>
          <span className="brand-mark" aria-hidden="true">
            <Boxes size={18} />
          </span>
          PocketCloud
        </div>
        <h1 className="page-title" style={{ fontSize: 22 }}>
          {title}
        </h1>
        {children}
      </main>
    </div>
  );
}

export function LoginPage() {
  const { t } = useTranslation();
  const { user, signIn } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [setupNeeded, setSetupNeeded] = useState(false);

  useEffect(() => {
    api
      .setupNeeded()
      .then((result) => setSetupNeeded(result.needed))
      .catch(() => {
        // signing in shows whether the panel is reachable
      });
  }, []);

  const target = (location.state as { from?: string } | null)?.from ?? "/";

  if (user) {
    return <Navigate to={target} replace />;
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await signIn(email, password);
      navigate(target, { replace: true });
    } catch {
      setError(t("auth.failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthCard title={t("auth.signIn")}>
      {setupNeeded && <Banner tone="warn">{t("auth.setupNeeded")}</Banner>}
      {error && <Banner tone="err">{error}</Banner>}
      <form
        onSubmit={(event) => void submit(event)}
        style={{ display: "flex", flexDirection: "column", gap: 12 }}
      >
        <Field label={t("auth.email")} htmlFor="email">
          <input
            id="email"
            className="input"
            type="email"
            autoComplete="username"
            required
            value={email}
            onChange={(event) => setEmail(event.target.value)}
          />
        </Field>
        <Field label={t("auth.password")} htmlFor="password">
          <input
            id="password"
            className="input"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </Field>
        <Button type="submit" variant="primary" disabled={busy}>
          {t("auth.signIn")}
        </Button>
      </form>
    </AuthCard>
  );
}

/** Creates the first admin account, with the token from the panel's log. */
export function SetupPage() {
  const { t } = useTranslation();
  const { user, signIn } = useAuth();
  const navigate = useNavigate();
  const [search] = useSearchParams();

  const [token, setToken] = useState(search.get("token") ?? "");
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  if (user) {
    return <Navigate to="/" replace />;
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.setup({ token, email, name, password });
      await signIn(email, password);
      navigate("/", { replace: true });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthCard title={t("auth.setupTitle")}>
      <p className="muted">{t("auth.setupText")}</p>
      {error && <Banner tone="err">{error}</Banner>}
      <form
        onSubmit={(event) => void submit(event)}
        style={{ display: "flex", flexDirection: "column", gap: 12 }}
      >
        <Field label={t("auth.setupToken")} htmlFor="token">
          <input
            id="token"
            className="input mono"
            required
            value={token}
            onChange={(event) => setToken(event.target.value)}
          />
        </Field>
        <Field label={t("auth.name")} htmlFor="name">
          <input
            id="name"
            className="input"
            autoComplete="name"
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </Field>
        <Field label={t("auth.email")} htmlFor="email">
          <input
            id="email"
            className="input"
            type="email"
            autoComplete="username"
            required
            value={email}
            onChange={(event) => setEmail(event.target.value)}
          />
        </Field>
        <Field label={t("auth.password")} htmlFor="password">
          <input
            id="password"
            className="input"
            type="password"
            autoComplete="new-password"
            required
            minLength={8}
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </Field>
        <Button type="submit" variant="primary" disabled={busy}>
          {t("auth.setupSubmit")}
        </Button>
      </form>
    </AuthCard>
  );
}
