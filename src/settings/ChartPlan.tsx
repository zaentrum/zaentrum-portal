import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { Badge, Button, Spinner, Table, Text, Textarea } from '@nalet/design-system';
import type { TableColumn } from '@nalet/design-system';
import { Braces, ListTree, X } from 'lucide-react';
import type { AddonChart, ChartComponentStatus, ChartWorkload } from '../lib/api';
import { chartPhaseTone, componentsProgress, phaseLabel, planCurrent, portLabel } from '../lib/addons';
import { parseSchema, parseValuesText, schemaNodes, secretPaths, setPath, type Values } from '../lib/valuesForm';
import { ValuesForm } from './ValuesForm';

// ChartPlan renders what the operator planned for a chart addon: the chart,
// what refuses it, what changes against the running chart, and the workloads
// and objects it would apply. Nothing in a plan runs until it is installed.
export function ChartPlan({ chart }: { chart: AddonChart }) {
  const plan = chart.plan;
  const current = planCurrent(chart);
  const violations = plan?.violations ?? [];
  const valuesErrors = plan?.valuesErrors ?? [];
  const changes = plan?.changes;
  const changed = !!changes && [changes.added, changes.removed, changes.images].some((l) => (l?.length ?? 0) > 0);
  return (
    <div className="set__plan">
      <div className="set__plan-head">
        <b>{plan?.chart.name || chart.name}</b>
        {plan?.chart.version && <span className="set__mono">{plan.chart.version}</span>}
        {plan?.chart.appVersion && <Text variant="dim">app {plan.chart.appVersion}</Text>}
        <Badge tone={chartPhaseTone(chart.phase)} dot>
          {phaseLabel(chart.phase)}
        </Badge>
        {!current && (
          <span className="set__planning">
            <Spinner size={13} /> <Text variant="dim">the operator is planning the current configuration…</Text>
          </span>
        )}
      </div>
      {plan?.chart.description && <Text variant="muted">{plan.chart.description}</Text>}
      {chart.message && <Text variant="dim">{chart.message}</Text>}

      {violations.length > 0 && (
        <Notice tone="refused" title="refused — the platform will not install this chart">
          {violations}
        </Notice>
      )}
      {valuesErrors.length > 0 && (
        <Notice tone="warning" title="values errors — set or fix these inputs, then it plans again">
          {valuesErrors}
        </Notice>
      )}
      {changed && (
        <div className="set__changes">
          <b>changes against the running chart</b>
          <ChangeList sign="+" items={changes?.added} />
          <ChangeList sign="−" items={changes?.removed} />
          <ChangeList sign="~" items={changes?.images} />
        </div>
      )}
      {plan && <Workloads workloads={plan.workloads ?? []} />}
      {plan && (plan.objects?.length ?? 0) > 0 && (
        <div>
          <Text variant="dim">objects: </Text>
          <span className="set__objects">
            {plan.objects?.map((o) => (
              <span key={`${o.kind}/${o.name}`} className="set__mono">
                {o.kind}/{o.name}
              </span>
            ))}
          </span>
        </div>
      )}
    </div>
  );
}

function Notice({ tone, title, children }: { tone: 'refused' | 'warning'; title: string; children: string[] }) {
  return (
    <div className={`set__notice set__notice--${tone}`} role={tone === 'refused' ? 'alert' : undefined}>
      <b>{title}</b>
      <ul className="set__list">
        {children.map((line) => (
          <li key={line} className="set__mono">
            {line}
          </li>
        ))}
      </ul>
    </div>
  );
}

function ChangeList({ sign, items }: { sign: string; items?: string[] | null }) {
  if (!items?.length) return null;
  return (
    <ul className="set__list">
      {items.map((line) => (
        <li key={sign + line} className="set__mono">
          {sign} {line}
        </li>
      ))}
    </ul>
  );
}

function Workloads({ workloads }: { workloads: ChartWorkload[] }) {
  const cols: TableColumn<ChartWorkload>[] = [
    {
      key: 'name',
      header: 'workload',
      render: (w) => (
        <span className="set__addon">
          <b>{w.name}</b>
          <Badge tone="neutral">{w.kind.toLowerCase()}</Badge>
        </span>
      ),
    },
    {
      key: 'images',
      header: 'images',
      render: (w) => (
        <span className="set__images">
          {(w.images ?? []).map((img) => (
            <span key={img} className="set__mono">
              {img}
            </span>
          ))}
        </span>
      ),
    },
    {
      key: 'ports',
      header: 'ports',
      render: (w) => <span className="set__mono">{(w.ports ?? []).map(portLabel).join(', ') || '—'}</span>,
    },
  ];
  return (
    <Table
      columns={cols}
      rows={workloads}
      rowKey={(w) => `${w.kind}/${w.name}`}
      dense
      empty={<Text variant="muted">the chart renders no workloads.</Text>}
    />
  );
}

// ChartProgress shows a chart addon's workloads coming up.
export function ChartProgress({ chart, children }: { chart: AddonChart; children?: ReactNode }) {
  const cols: TableColumn<ChartComponentStatus>[] = [
    { key: 'name', header: 'workload', render: (c) => <b>{c.name}</b> },
    {
      key: 'ready',
      header: 'ready',
      align: 'right',
      render: (c) => (
        <Badge tone={c.desired > 0 && c.ready >= c.desired ? 'green' : c.reason ? 'amber' : 'blue'} dot>
          {c.ready}/{c.desired}
        </Badge>
      ),
    },
    { key: 'reason', header: 'reason', render: (c) => <Text variant="dim">{c.reason || '—'}</Text> },
  ];
  return (
    <div className="set__plan">
      <div className="set__plan-head">
        <b>{chart.name}</b>
        <Badge tone={chartPhaseTone(chart.phase)} dot>
          {phaseLabel(chart.phase)}
        </Badge>
        <Text variant="dim">{componentsProgress(chart.components)}</Text>
        <Badge tone={chart.registered ? 'green' : 'neutral'}>{chart.registered ? 'registered' : 'not registered yet'}</Badge>
      </div>
      {chart.message && <Text variant="dim">{chart.message}</Text>}
      {chart.registrationError && <span className="set__err">registration: {chart.registrationError}</span>}
      <Table columns={cols} rows={chart.components ?? []} rowKey={(c) => c.name} dense empty={<Text variant="muted">no workloads reported yet.</Text>} />
      {children}
    </div>
  );
}

// ChartValues edits a chart addon's values: the form its schema describes, or
// the values as JSON — for a chart without a schema, and for values the form
// does not cover. Secret inputs exist only in the form.
export function ChartValues({
  chart,
  values,
  secretKeys,
  disabled,
  onValues,
  onSecret,
  onClearSecret,
}: {
  chart: AddonChart;
  values: Values;
  secretKeys: string[];
  disabled?: boolean;
  onValues: (next: Values) => void;
  onSecret: (path: string, value: string) => void;
  onClearSecret: (path: string) => void;
}) {
  const { schema, error: schemaError } = useMemo(() => parseSchema(chart.plan?.valuesSchema), [chart.plan?.valuesSchema]);
  const nodes = useMemo(() => schemaNodes(schema), [schema]);
  const [mode, setMode] = useState<'form' | 'json'>('form');
  const [jsonDraft, setJsonDraft] = useState<string | null>(null);
  const [jsonError, setJsonError] = useState<string | null>(null);
  const showForm = nodes.length > 0 && mode === 'form';
  // Secret inputs set through zae or an earlier schema that this form does
  // not show can still be cleared.
  const formSecrets = new Set(secretPaths(nodes));
  const otherSecrets = secretKeys.filter((k) => !formSecrets.has(k));

  return (
    <div className="set__values-editor">
      <div className="set__checklist-head">
        <b>values</b>
        {!chart.plan && <Text variant="dim">the form appears once the chart is planned</Text>}
        {schemaError && <span className="set__err">{schemaError}</span>}
        {nodes.length > 0 && (
          <Button
            variant="ghost"
            size="sm"
            leading={mode === 'form' ? <Braces size={13} /> : <ListTree size={13} />}
            onClick={() => {
              setMode(mode === 'form' ? 'json' : 'form');
              setJsonDraft(null);
              setJsonError(null);
            }}
          >
            {mode === 'form' ? 'edit as JSON' : 'edit as form'}
          </Button>
        )}
      </div>
      {showForm ? (
        <ValuesForm
          nodes={nodes}
          values={values}
          secretKeys={secretKeys}
          disabled={disabled}
          onValues={(path, value) => onValues(setPath(values, path, value))}
          onSecret={onSecret}
          onClearSecret={onClearSecret}
        />
      ) : (
        (chart.plan || Object.keys(values).length > 0) && (
          <>
            <Textarea
              rows={8}
              value={jsonDraft ?? (Object.keys(values).length ? JSON.stringify(values, null, 2) : '')}
              placeholder={'{\n  "worker": { "replicas": 2 }\n}'}
              disabled={disabled}
              invalid={!!jsonError}
              aria-label="values as JSON"
              onChange={(e) => setJsonDraft(e.target.value)}
              onBlur={() => {
                if (jsonDraft === null) return;
                const r = parseValuesText(jsonDraft);
                if (r.error) {
                  setJsonError(r.error);
                  return;
                }
                setJsonError(null);
                setJsonDraft(null);
                onValues(r.values ?? {});
              }}
            />
            {jsonError && <span className="set__err">{jsonError}</span>}
            <Text variant="dim">non-secret values only — secret inputs are set in the form.</Text>
          </>
        )
      )}
      {otherSecrets.length > 0 && (
        <div className="set__secret">
          <Text variant="dim">other secret inputs set:</Text>
          {otherSecrets.map((k) => (
            <span key={k} className="set__secret">
              <span className="set__mono">{k}</span>
              <Button variant="ghost" size="sm" leading={<X size={13} />} disabled={disabled} onClick={() => onClearSecret(k)}>
                clear
              </Button>
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
