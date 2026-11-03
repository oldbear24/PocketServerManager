import { Layers, Plus } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";

import { PageHead } from "../components/Layout";
import { useToast } from "../components/toast";
import { Button, Card, EmptyState, Field, StatusPill, Table, type Tone } from "../components/ui";
import { errorMessage } from "../lib/api";
import { hasRole, useAuth } from "../lib/auth";
import { formatDateTime } from "../lib/format";
import { pb } from "../lib/pb";
import { useLiveList } from "../lib/realtime";
import { stackApi, stackNamePattern, type ComposeProject } from "../lib/stacks";
import type { Command, Container, Host, Stack } from "../lib/types";

const newStackTemplate = `services:
  web:
    image: nginx:alpine
    restart: unless-stopped
`;

export function stackTone(status: string): Tone {
  switch (status) {
    case "deployed":
    case "imported":
      return "ok";
    case "failed":
      return "err";
    case "deploying":
      return "info";
    default:
      return "off";
  }
}

function RunningPill({ containers }: { containers: Container[] }) {
  const { t } = useTranslation();
  const running = containers.filter((container) => container.state === "running").length;

  return (
    <StatusPill
      tone={containers.length === 0 ? "off" : running === containers.length ? "ok" : "warn"}
    >
      {t("stacks.running", { running, total: containers.length })}
    </StatusPill>
  );
}

/** A form for a new, empty stack on a server; the editor takes it from there. */
function NewStackCard({ hosts, onCancel }: { hosts: Host[]; onCancel: () => void }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [host, setHost] = useState(hosts[0]?.id ?? "");
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const nameError = name && !stackNamePattern.test(name) ? t("stacks.nameInvalid") : "";

  const create = async () => {
    setBusy(true);
    setError("");
    try {
      const stack = await pb.collection("stacks").create<Stack>({
        host: host || hosts[0]?.id,
        name,
        compose_yaml: newStackTemplate,
        env: "",
      });
      void navigate(`/stacks/${stack.id}`);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Card title={t("stacks.new")}>
      <div className="form-grid">
        <Field label={t("stacks.name")} htmlFor="stack-name" error={nameError}>
          <input
            id="stack-name"
            className="input mono"
            value={name}
            autoComplete="off"
            onChange={(event) => setName(event.target.value)}
          />
        </Field>
        <Field label={t("stacks.server")} htmlFor="stack-host">
          <select
            id="stack-host"
            className="select"
            style={{ width: "100%" }}
            value={host}
            onChange={(event) => setHost(event.target.value)}
          >
            {hosts.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name}
              </option>
            ))}
          </select>
        </Field>
      </div>
      {error && (
        <p className="field-error" style={{ marginTop: 8 }}>
          {error}
        </p>
      )}
      <div style={{ display: "flex", gap: 8, marginTop: 14 }}>
        <Button
          variant="primary"
          disabled={busy || !name || Boolean(nameError) || hosts.length === 0}
          onClick={() => void create()}
        >
          {t("stacks.create")}
        </Button>
        <Button onClick={onCancel}>{t("common.cancel")}</Button>
      </div>
    </Card>
  );
}

/** Compose projects on the servers that no stack manages yet. */
function ImportCard({ hosts, canAct }: { hosts: Host[]; canAct: boolean }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [projects, setProjects] = useState<(ComposeProject & { host: Host })[]>([]);

  const imports = useLiveList<Command>("commands", {
    filter: "type = 'stack.import'",
    sort: "-created",
    perPage: 1,
  });

  const hostKey = hosts.map((host) => host.id).join(",");
  const lastImport = imports.items[0]?.id;
  const lastStatus = imports.items[0]?.status;

  useEffect(() => {
    let cancelled = false;

    Promise.all(
      hosts.map((host) =>
        stackApi
          .composeProjects(host.id)
          .then((found) => found.map((project) => ({ ...project, host }))),
      ),
    )
      .then((lists) => {
        if (!cancelled) {
          setProjects(lists.flat());
        }
      })
      .catch(() => {
        // the card only stays empty; this list is a help and not the page's content
      });

    return () => {
      cancelled = true;
    };
    // a finished import takes a project off the list
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hostKey, lastImport, lastStatus]);

  if (projects.length === 0) {
    return null;
  }

  const importProject = async (project: ComposeProject & { host: Host }) => {
    try {
      await stackApi.importProject(project.host.id, project.project);
      toast.success(t("stacks.importQueued", { name: project.project }));
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <Card title={t("stacks.found")} note={t("stacks.foundNote")} flush>
      <Table label={t("stacks.found")}>
        <thead>
          <tr>
            <th>{t("stacks.project")}</th>
            <th>{t("stacks.server")}</th>
            <th>{t("stacks.services")}</th>
            <th>{t("host.state")}</th>
            {canAct && <th style={{ textAlign: "right" }}>{t("common.actions")}</th>}
          </tr>
        </thead>
        <tbody>
          {projects.map((project) => (
            <tr key={`${project.host.id}/${project.project}`}>
              <td>
                <div className="cell-title">{project.project}</div>
                <div className="cell-sub mono">{project.config_files[0] ?? "–"}</div>
              </td>
              <td className="muted">{project.host.name}</td>
              <td className="muted">{project.services.join(", ")}</td>
              <td className="num">
                {t("stacks.running", { running: project.running, total: project.total })}
              </td>
              {canAct && (
                <td style={{ textAlign: "right" }}>
                  <Button size="small" onClick={() => void importProject(project)}>
                    {t("stacks.import")}
                  </Button>
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </Table>
    </Card>
  );
}

export function StacksPage() {
  const { t } = useTranslation();
  const { user } = useAuth();
  const [creating, setCreating] = useState(false);

  const stacks = useLiveList<Stack>("stacks", { sort: "name" });
  const hosts = useLiveList<Host>("hosts", { sort: "name" });
  const containers = useLiveList<Container>("containers", {
    filter: "project != ''",
    perPage: 500,
  });

  const canAct = hasRole(user, "operator");

  const hostNames = new Map(hosts.items.map((host) => [host.id, host.name]));

  return (
    <>
      <PageHead
        title={t("nav.stacks")}
        actions={
          canAct && (
            <Button variant="primary" onClick={() => setCreating(true)}>
              <Plus size={16} aria-hidden="true" />
              {t("stacks.new")}
            </Button>
          )
        }
      />

      {creating && <NewStackCard hosts={hosts.items} onCancel={() => setCreating(false)} />}

      <Card flush>
        {stacks.items.length === 0 && !stacks.loading ? (
          <EmptyState icon={Layers} title={t("stacks.noneTitle")}>
            {t("stacks.noneText")}
          </EmptyState>
        ) : (
          <Table label={t("nav.stacks")}>
            <thead>
              <tr>
                <th>Stack</th>
                <th>{t("stacks.server")}</th>
                <th>{t("host.state")}</th>
                <th>{t("stacks.revision")}</th>
                <th>{t("stacks.deployedAt")}</th>
              </tr>
            </thead>
            <tbody>
              {stacks.items.map((stack) => (
                <tr key={stack.id}>
                  <td>
                    <Link className="cell-title" to={`/stacks/${stack.id}`}>
                      {stack.name}
                    </Link>
                  </td>
                  <td className="muted">{hostNames.get(stack.host) ?? "–"}</td>
                  <td>
                    <RunningPill
                      containers={containers.items.filter(
                        (container) =>
                          container.host === stack.host && container.project === stack.name,
                      )}
                    />
                  </td>
                  <td className="num">
                    {stack.deployed_revision ? `r${stack.deployed_revision}` : "–"}
                    {stack.current_revision > stack.deployed_revision && (
                      <span className="warn-text"> → r{stack.current_revision}</span>
                    )}
                  </td>
                  <td className="muted">{formatDateTime(stack.deployed_at)}</td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>

      <ImportCard hosts={hosts.items} canAct={canAct} />
    </>
  );
}
