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
import { ContainerDetailPage } from "./pages/ContainerDetail";
import { DatabasesPage } from "./pages/Databases";
import { DomainsPage } from "./pages/Domains";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { LoginPage, SetupPage } from "./pages/Login";
import { OverviewPage } from "./pages/Overview";
import { ServerDetailPage } from "./pages/ServerDetail";
import { ServersPage } from "./pages/Servers";
import { SettingsPage } from "./pages/Settings";
import { SiteNewPage } from "./pages/SiteNew";
import { SitesPage } from "./pages/Sites";
import { StackDetailPage } from "./pages/StackDetail";
import { StacksPage } from "./pages/Stacks";
import { TerminalPage } from "./pages/Terminal";

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
        <Route path="servers/:id/containers/:docker" element={<ContainerDetailPage />} />
        <Route
          path="servers/:id/terminal"
          element={
            <RequireRole role="admin">
              <TerminalPage />
            </RequireRole>
          }
        />
        <Route path="stacks" element={<StacksPage />} />
        <Route path="stacks/:id" element={<StackDetailPage />} />
        <Route path="databases" element={<DatabasesPage />} />
        <Route path="backups" element={<ComingLater feature="backups" milestone="M4" />} />
        <Route path="sites" element={<SitesPage />} />
        <Route path="sites/new" element={<SiteNewPage />} />
        <Route path="sites/:id" element={<SitesPage />} />
        <Route path="domains" element={<DomainsPage />} />
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
