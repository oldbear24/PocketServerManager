import { useState } from "react";
import { AlertCircle, CheckCircle2, Settings, XCircle } from "lucide-react";

import { PageHead } from "../components/Layout";
import {
  Banner,
  Button,
  Card,
  CodeBlock,
  EmptyState,
  Field,
  IconButton,
  IssueBanner,
  Kpi,
  ProgressBar,
  SegmentedControl,
  StatusPill,
  Table,
  Tabs,
  Timeline,
} from "../components/ui";

/**
 * Every base component in every state, in one place. The accessibility tests run
 * axe against this page; it is also handy while working on the design tokens.
 */
export function ComponentDemoPage() {
  const [tab, setTab] = useState("one");
  const [segment, setSegment] = useState("a");

  return (
    <>
      <PageHead
        crumbs="System / Components"
        title="Components"
        description="All base components."
      />

      <div className="kpis">
        <Kpi
          label="Agents"
          hint="gRPC"
          value={
            <>
              6 <small>/ 7</small>
            </>
          }
          note="One is offline"
        />
        <Kpi
          label="Integrity check"
          value="OK"
          tone="ok"
          icon={<CheckCircle2 size={24} aria-hidden="true" />}
          note="restic check · Sunday 06:00"
        />
        <Kpi
          label="Integrity check"
          value="Errors"
          tone="err"
          icon={<XCircle size={24} aria-hidden="true" />}
          note="1 target has errors"
        />
        <Kpi
          label="Containers"
          hint="11 stacks"
          to="/stacks"
          value={
            <>
              38 <small>running</small>
            </>
          }
          note={
            <>
              3 stopped · <span className="warn-text">1 restarting</span>
            </>
          }
        />
      </div>

      <Card title="Buttons" note="note" side="side">
        <div className="filters">
          <Button>Default</Button>
          <Button variant="primary">Primary</Button>
          <Button variant="danger">Danger</Button>
          <Button variant="warning">Warning</Button>
          <Button variant="critical">Critical</Button>
          <Button size="small">Small</Button>
          <Button disabled>Disabled</Button>
          <IconButton aria-label="Settings">
            <Settings size={16} aria-hidden="true" />
          </IconButton>
        </div>
      </Card>

      <Card title="Status">
        <div className="filters">
          <StatusPill tone="ok">Online</StatusPill>
          <StatusPill tone="warn">Degraded</StatusPill>
          <StatusPill tone="err">Offline</StatusPill>
          <StatusPill tone="off">Stopped</StatusPill>
          <StatusPill tone="info" dot={false}>
            sent
          </StatusPill>
        </div>
      </Card>

      <Banner tone="warn">A warning banner.</Banner>
      <Banner tone="err">An error banner.</Banner>

      {(["err", "warn", "info"] as const).map((tone) => (
        <IssueBanner
          key={tone}
          id={`demo-issue-${tone}`}
          tone={tone}
          icon={<AlertCircle size={20} aria-hidden="true" />}
          title={`An issue banner (${tone})`}
          detail="repository is already locked by PID 4121 on nas (02:58)"
          meta="attempt 1 of 4 · next try: 03:35"
          actions={
            <>
              {tone !== "info" && <Button variant="critical">Unlock and retry</Button>}
              <Button>Show log</Button>
            </>
          }
        />
      ))}

      <Card title="Table" flush>
        <Table label="Example table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Usage</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td className="cell-title">vps-hetzner</td>
              <td>
                <ProgressBar value={22} label="Usage of vps-hetzner" />
              </td>
            </tr>
            <tr>
              <td className="cell-title">nas</td>
              <td>
                <ProgressBar value={85} label="Usage of nas" />
              </td>
            </tr>
            <tr>
              <td className="cell-title">rpi</td>
              <td>
                <ProgressBar value={95} label="Usage of rpi" />
              </td>
            </tr>
          </tbody>
        </Table>
      </Card>

      <Card title="Meters on their own track" flush>
        <Table label="Example meters">
          <thead>
            <tr>
              <th>Name</th>
              <th>CPU</th>
            </tr>
          </thead>
          <tbody>
            {[
              { name: "vps-hetzner", value: 22 },
              { name: "nas", value: 81 },
              { name: "vps-wedos", value: 93 },
            ].map(({ name, value }) => (
              <tr key={name}>
                <td className="cell-title">{name}</td>
                <td>
                  <span className="num">{value} %</span>
                  <ProgressBar
                    value={value}
                    label={`CPU of ${name}`}
                    warnAt={75}
                    errAt={90}
                    track="ramp"
                    valueText={value >= 75 ? `${value} %, high usage` : undefined}
                  />
                </td>
              </tr>
            ))}
            <tr>
              <td className="cell-title">rpi-garaz</td>
              <td>
                <span className="num faint">—</span>
                <div className="progress empty" aria-hidden="true" />
              </td>
            </tr>
          </tbody>
        </Table>
      </Card>

      <Tabs
        label="Example tabs"
        active={tab}
        onChange={setTab}
        items={[
          { id: "one", label: "One", count: 3 },
          { id: "two", label: "Two" },
        ]}
      />

      <SegmentedControl
        label="Example choice"
        value={segment}
        onChange={setSegment}
        options={[
          { value: "a", label: "A" },
          { value: "b", label: "B" },
        ]}
      />

      <Card title="Code" flush>
        <CodeBlock label="Example command" code="docker run -d nginx" />
      </Card>

      <Card title="Timeline">
        <Timeline
          label="Example steps"
          items={[
            { id: "a", state: "done", title: "Done", note: "note", side: "12:31" },
            { id: "b", state: "active", title: "Active" },
            { id: "c", state: "failed", title: "Failed" },
            { id: "d", state: "pending", title: "Pending" },
          ]}
        />
      </Card>

      <Card title="Form">
        <Field label="Name" htmlFor="demo-name" error="This is required">
          <input id="demo-name" className="input" />
        </Field>
      </Card>

      <Card>
        <EmptyState title="Nothing here" action={<Button>Do something</Button>}>
          An empty state.
        </EmptyState>
      </Card>
    </>
  );
}
