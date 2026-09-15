import { useEffect, useState } from 'react';
import { Button, Checkbox, Field, Input, Modal, Text } from '@nalet/design-system';
import { CircleCheck, ListChecks, Rocket, Save, Trash2, Undo2 } from 'lucide-react';
import { usePortalApi, type InstalledAddon } from '../lib/api';
import { installBlockers, planCurrent, upgradePlanned } from '../lib/addons';
import type { Values } from '../lib/valuesForm';
import { ChartPlan, ChartProgress, ChartValues } from './ChartPlan';
import { CHART_POLL_MS, errText, useAddonChart } from './useAddonChart';

const path = (name: string) => `/addon-charts/${encodeURIComponent(name)}`;

// UpgradeChartDialog moves a chart addon to another version (or chart link):
// the new chart is planned first, its changes against the running one shown,
// and applied only when the admin says so. Cancelling returns the addon to the
// chart it runs.
export function UpgradeChartDialog({ addon, onClose, onChanged }: { addon: InstalledAddon; onClose: () => void; onChanged: () => void }) {
  const api = usePortalApi();
  const [stage, setStage] = useState<'choose' | 'plan' | 'apply'>(upgradePlanned(addon) ? 'plan' : 'choose');
  const { chart, error: loadErr, reload } = useAddonChart(addon.key, stage === 'choose' ? 0 : CHART_POLL_MS);
  const running = chart?.lastAppliedChart ?? addon.chart?.lastApplied ?? null;
  const oci = (chart?.chart.ref ?? addon.chart?.ref ?? '').toLowerCase().startsWith('oci://');
  const [version, setVersion] = useState('');
  const [link, setLink] = useState('');
  const [digest, setDigest] = useState('');
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const done =
    stage === 'apply' && !!chart && chart.phase === 'Ready' && !chart.suspended && chart.registered &&
    (chart.lastAppliedChart?.version ?? '') === (chart.chart.version ?? '') && chart.lastAppliedChart?.ref === chart.chart.ref;
  useEffect(() => {
    if (done) onChanged();
  }, [done, onChanged]);

  async function run(label: string, fn: () => Promise<void>) {
    setBusy(label);
    setErr(null);
    try {
      await fn();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(null);
    }
  }

  const plan = () =>
    run('plan', async () => {
      const body: Record<string, unknown> = oci ? { version: version.trim() } : { chart: link.trim() };
      if (digest.trim()) body.digest = digest.trim();
      await api(path(addon.key), { method: 'PATCH', body: JSON.stringify(body) });
      setStage('plan');
      onChanged();
      await reload();
    });

  const apply = () =>
    run('apply', async () => {
      await api(`${path(addon.key)}/install`, { method: 'POST' });
      setStage('apply');
      onChanged();
      await reload();
    });

  // Back to the chart that runs, and reconciling again.
  const revert = () =>
    run('revert', async () => {
      if (running) {
        await api(path(addon.key), {
          method: 'PATCH',
          body: JSON.stringify({ chart: running.ref, version: running.version ?? '', digest: running.digest ?? '', suspend: false }),
        });
        onChanged();
      }
      onClose();
    });

  const blockers = installBlockers(chart);
  const canApply = stage === 'plan' && planCurrent(chart) && blockers.length === 0;
  const footer =
    stage === 'choose' ? (
      <>
        <Button variant="ghost" size="sm" onClick={onClose}>
          cancel
        </Button>
        <Button size="sm" leading={<ListChecks size={14} />} loading={busy === 'plan'} disabled={oci ? !version.trim() : !link.trim()} onClick={plan}>
          plan
        </Button>
      </>
    ) : stage === 'plan' ? (
      <>
        <Button variant="ghost" size="sm" leading={<Undo2 size={13} />} loading={busy === 'revert'} onClick={revert}>
          cancel upgrade
        </Button>
        <Button size="sm" leading={<Rocket size={14} />} loading={busy === 'apply'} disabled={!canApply} title={canApply ? undefined : blockers.join('; ')} onClick={apply}>
          apply
        </Button>
      </>
    ) : (
      <Button size="sm" variant={done ? undefined : 'ghost'} onClick={onClose}>
        {done ? 'done' : 'close — it keeps upgrading'}
      </Button>
    );

  return (
    <Modal open width={880} closeOnBackdrop={false} onClose={onClose} title={`upgrade ${addon.title || addon.key}`} footer={footer}>
      <div className="set__form">
        <Text variant="muted" as="p">
          runs <span className="set__mono">{running ? `${running.ref} ${running.version ?? ''}` : '—'}</span>
        </Text>
        {stage === 'choose' &&
          (oci ? (
            <div className="set__row">
              <Field label="version" hint="the chart version to move to" required>
                <Input value={version} autoFocus placeholder={running?.version ?? ''} onChange={(e) => setVersion(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && version.trim() && plan()} />
              </Field>
              <Field label="digest" hint="optional: pins the chart archive">
                <Input value={digest} placeholder="sha256:…" onChange={(e) => setDigest(e.target.value)} />
              </Field>
            </div>
          ) : (
            <div className="set__row">
              <Field label="chart link" hint="an https:// link to the new chart archive" required>
                <Input value={link} autoFocus placeholder={running?.ref ?? ''} onChange={(e) => setLink(e.target.value)} />
              </Field>
              <Field label="digest" hint="optional: pins the chart archive">
                <Input value={digest} placeholder="sha256:…" onChange={(e) => setDigest(e.target.value)} />
              </Field>
            </div>
          ))}
        {stage === 'plan' && chart && (
          <>
            <Text variant="dim" as="p">
              planned: <span className="set__mono">{`${chart.chart.ref} ${chart.chart.version ?? ''}`}</span> — nothing changes until you
              apply it. Closing keeps the plan; until you apply or cancel it, the operator does not reconcile the addon.
            </Text>
            <ChartPlan chart={chart} />
          </>
        )}
        {stage === 'apply' && chart && (
          <ChartProgress chart={chart}>
            {done && (
              <span className="set__ok set__done">
                <CircleCheck size={14} /> {addon.key} runs {chart.chart.version || chart.chart.ref}.
              </span>
            )}
          </ChartProgress>
        )}
        {loadErr && <span className="set__err">{loadErr}</span>}
        {err && <span className="set__err">{err}</span>}
      </div>
    </Modal>
  );
}

// ChartValuesDialog edits a chart addon's values and secret inputs. Saving
// replaces the values and applies them: an installed addon rolls out with the
// new values once the operator has planned them.
export function ChartValuesDialog({ addon, onClose, onChanged }: { addon: InstalledAddon; onClose: () => void; onChanged: () => void }) {
  const api = usePortalApi();
  const [saved, setSaved] = useState(false);
  const { chart, error: loadErr, reload } = useAddonChart(addon.key, saved ? CHART_POLL_MS : 0);
  const [values, setValues] = useState<Values | null>(null);
  const [secrets, setSecrets] = useState<Record<string, string>>({});
  const [clears, setClears] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (chart && values === null) setValues(chart.values ?? {});
  }, [chart, values]);

  const dirty =
    !!chart && values !== null &&
    (JSON.stringify(values) !== JSON.stringify(chart.values ?? {}) || Object.keys(secrets).length > 0 || clears.length > 0);
  const secretKeys = [...new Set([...(chart?.secretKeys ?? []), ...Object.keys(secrets)])].filter((k) => !clears.includes(k));

  async function save() {
    if (!values) return;
    setBusy(true);
    setErr(null);
    try {
      const body: Record<string, unknown> = { values: Object.keys(values).length ? values : null };
      if (Object.keys(secrets).length) body.secretValues = secrets;
      const cleared = clears.filter((k) => chart?.secretKeys.includes(k));
      if (cleared.length) body.clearSecrets = cleared;
      await api(path(addon.key), { method: 'PATCH', body: JSON.stringify(body) });
      setSecrets({});
      setClears([]);
      setSaved(true);
      onChanged();
      await reload();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  }

  const blockers = saved ? installBlockers(chart) : [];
  const settled = saved && planCurrent(chart);
  return (
    <Modal
      open
      width={760}
      onClose={onClose}
      title={`values of ${addon.title || addon.key}`}
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            {saved && !dirty ? 'close' : 'cancel'}
          </Button>
          <Button size="sm" leading={<Save size={14} />} loading={busy} disabled={!dirty} onClick={save}>
            save
          </Button>
        </>
      }
    >
      <div className="set__form">
        <Text variant="muted" as="p">
          {addon.suspended
            ? 'the addon is planned, not applied: saving plans it again.'
            : 'saving applies the values: the operator plans them and rolls the addon out with them.'}{' '}
          Secret inputs go to the addon’s values Secret and are never shown again.
        </Text>
        {!chart && !loadErr && <Text variant="dim">reading {addon.key}…</Text>}
        {chart && values !== null && (
          <ChartValues
            chart={chart}
            values={values}
            secretKeys={secretKeys}
            disabled={busy}
            onValues={setValues}
            onSecret={(p, v) => {
              setSecrets((s) => ({ ...s, [p]: v }));
              setClears((c) => c.filter((k) => k !== p));
            }}
            onClearSecret={(p) => {
              setClears((c) => (c.includes(p) ? c : [...c, p]));
              setSecrets((s) => {
                const next = { ...s };
                delete next[p];
                return next;
              });
            }}
          />
        )}
        {Object.keys(secrets).length + clears.length > 0 && (
          <Text variant="dim">
            unsaved: {[...Object.keys(secrets).map((k) => `set ${k}`), ...clears.map((k) => `clear ${k}`)].join(', ')}
          </Text>
        )}
        {saved && !settled && <Text variant="dim">saved — the operator is planning the new values…</Text>}
        {settled && blockers.length === 0 && (
          <span className="set__ok set__done">
            <CircleCheck size={14} /> saved and planned{addon.suspended ? '' : ' — the addon rolls out with them'}.
          </span>
        )}
        {settled && blockers.length > 0 && (
          <span className="set__err">the new values do not plan cleanly, and are not applied: {blockers.join('; ')}</span>
        )}
        {loadErr && <span className="set__err">{loadErr}</span>}
        {err && <span className="set__err">{err}</span>}
      </div>
    </Modal>
  );
}

// RemoveChartDialog removes a chart addon — or cancels one that was only
// planned. The operator's garbage collection deletes what the chart created.
export function RemoveChartDialog({
  addon,
  planOnly,
  onClose,
  onRemoved,
}: {
  addon: InstalledAddon;
  planOnly: boolean;
  onClose: () => void;
  onRemoved: (message: string) => void;
}) {
  const api = usePortalApi();
  const [keep, setKeep] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function remove() {
    setBusy(true);
    setErr(null);
    try {
      const r = await api<{ warnings?: string[] }>(`${path(addon.key)}?keepValues=${keep && !planOnly}`, { method: 'DELETE' });
      const warnings = r?.warnings ?? [];
      onRemoved(
        `${planOnly ? 'cancelled' : 'removed'} ${addon.key}${keep && !planOnly ? ' — its values Secrets are kept' : ''}${warnings.length ? ` (${warnings.join('; ')})` : ''}`,
      );
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(false);
    }
  }

  const values = `zaentrum-addon-${addon.key}-values`;
  const generated = `zaentrum-addon-${addon.key}-generated`;
  return (
    <Modal
      open
      onClose={onClose}
      title={`${planOnly ? 'cancel' : 'remove'} ${addon.title || addon.key}`}
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={onClose}>
            {planOnly ? 'keep the plan' : 'cancel'}
          </Button>
          <Button variant="danger" size="sm" leading={<Trash2 size={13} />} loading={busy} onClick={remove}>
            {planOnly ? 'cancel addon' : 'remove'}
          </Button>
        </>
      }
    >
      <div className="set__form">
        {planOnly ? (
          <Text as="p">It was planned and never installed. Cancelling deletes the plan and any secret inputs entered for it.</Text>
        ) : (
          <>
            <Text as="p">
              The operator deletes everything the chart installed — its workloads, services, volume claims and the data
              in them. The portal deletes the addon’s app, tiles and slot rows.
            </Text>
            <Field
              hint={`the Secrets ${values} (secret inputs) and ${generated} (values the operator generated) stay in the namespace, labelled zaentrum.io/keep=true; values that are not secret go with the addon. Unchecked, both Secrets are deleted too.`}
            >
              <Checkbox label="keep values" checked={keep} onChange={(e) => setKeep(e.target.checked)} />
            </Field>
          </>
        )}
        {err && <span className="set__err">{err}</span>}
      </div>
    </Modal>
  );
}
