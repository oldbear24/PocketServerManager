import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { BrowserRouter, Navigate, Route, Routes, useLocation } from "react-router-dom";

import { Layout } from "./components/Layout";
import { ToastProvider } from "./components/toast";
import { EmptyState } from "./components/ui";
import { AuthProvider, hasRole, useAuth } from "./lib/auth";
import type { Role } from "./lib/pb";
import { AgentsPage } from "./pages/Agents";
import { AuditPage } from "./pages/Audit";
import { ComingLater } from "./pages/ComingLater";
import { ComponentDemoPage } from "./pages/ComponentDemo";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { LoginPage, SetupPage } from "./pages/Login";
import { OverviewPage } from "./pages/Overview";
import { ServerDetailPage } from "./pages/ServerDetail";
import { ServersPage } from "./pages/Servers";
import { SettingsPage } from "./pages/Settings";

/** Sends anyone without a valid session to the sign-in page, and back afterwards. */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const location = useLocation();

  if (!user) {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  }

  return children;
}

/** Keeps a screen away from lower roles. The panel refuses their calls in any case. */
export function RequireRole({ role, children }: { role: Role; children: ReactNode }) {
  const { t } = useTranslation();
  const { user } = useAuth();

  if (!hasRole(user, role)) {
    return <EmptyState title={t("auth.forbiddenTitle")}>{t("auth.forbiddenText")}</EmptyState>;
  }

  return children;
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/setup" element={<SetupPage />} />
      <Route
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route index element={<OverviewPage />} />
        <Route path="servers" element={<ServersPage />} />
        <Route path="servers/:id" element={<ServerDetailPage />} />
        <Route path="servers/:id/:tab" element={<ServerDetailPage />} />
        <Route path="stacks" element={<ComingLater feature="stacks" milestone="M2" />} />
        <Route path="databases" element={<ComingLater feature="databases" milestone="M2" />} />
        <Route path="backups" element={<ComingLater feature="backups" milestone="M4" />} />
        <Route path="sites" element={<ComingLater feature="sites" milestone="M3" />} />
        <Route path="domains" element={<ComingLater feature="domains" milestone="M3" />} />
        <Route
          path="agents"
          element={
            <RequireRole role="admin">
              <AgentsPage />
            </RequireRole>
          }
        />
        <Route
          path="audit/:tab?"
          element={
            <RequireRole role="operator">
              <AuditPage />
            </RequireRole>
          }
        />
        <Route
          path="diagnostics"
          element={
            <RequireRole role="admin">
              <DiagnosticsPage />
            </RequireRole>
          }
        />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="components" element={<ComponentDemoPage />} />
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}

function NotFound() {
  const { t } = useTranslation();
  return <EmptyState title={t("common.notFound")} />;
}

export default function App() {
  return (
    <BrowserRouter>
      <AuthProvider>
        <ToastProvider>
          <AppRoutes />
        </ToastProvider>
      </AuthProvider>
    </BrowserRouter>
  );
}
