import { useEffect, useRef, useState } from 'react';
import { Badge, Button, Field, Input, Modal, Text, Textarea } from '@nalet/design-system';
import { CircleCheck, ListChecks, Rocket, Trash2 } from 'lucide-react';
import { ApiError, usePortalApi, type InstalledAddon } from '../lib/api';
import { chartReady, defaultChartName, installBlockers, isChartRef, isPlanOnly, planCurrent } from '../lib/addons';
import { parseValuesText, type Values } from '../lib/valuesForm';
import { ChartPlan, ChartProgress, ChartValues } from './ChartPlan';
import { CHART_POLL_MS, errText, useAddonChart } from './useAddonChart';

type Step = 'chart' | 'plan' | 'install';

const STEPS: { step: Step; label: string }[] = [
  { step: 'chart', label: '1 chart' },
  { step: 'plan', label: '2 plan' },
  { step: 'install', label: '3 install' },
];

// AddonChartWizard adds an addon from a Helm chart in three steps:
//
//  1. chart — a reference (oci:// with a version, or an https:// link to a
//     chart archive), optionally a digest, a name and values as JSON. portal-api
//     writes a suspended ZaentrumAddon: the operator plans it, applies nothing.
//  2. plan — what the chart would run and anything that refuses it, with the
//     form its values schema describes. Every completed change plans again.
//  3. install — suspend off; the workloads come up until the addon is ready
//     and registered.
//
// Closing keeps a planned addon: settings lists it, to review or cancel.
// resume opens step 2 for such an addon.
export function AddonChartWizard({
  resume,
  existing,
  onClose,
  onChanged,
  onUnsupported,
}: {
  resume?: string;
  existing: InstalledAddon[];
  onClose: () => void;
  onChanged: () => void;
  onUnsupported: (note: string) => void;
}) {
  const api = usePortalApi();
  const [step, setStep] = useState<Step>(resume ? 'plan' : 'chart');
  const [name, setName] = useState<string | null>(resume ?? null);
  const [ref, setRef] = useState('');
  const [version, setVersion] = useState('');
  const [digest, setDigest] = useState('');
  const [nameInput, setNameInput] = useState('');
  const [valuesText, setValuesText] = useState('');
  const [values, setValues] = useState<Values | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const { chart, error: loadErr, reload } = useAddonChart(step === 'chart' ? null : name, CHART_POLL_MS);
  // Changes are sent one after another: each carries all values, so the
  // last one written must be the last one made.
  const queue = useRef<Promise<void>>(Promise.resolve());
  const [pending, setPending] = useState(0);

  // The values being edited start as the addon's.
  useEffect(() => {
    if (chart && values === null) setValues(chart.values ?? {});
  }, [chart, values]);

  const ready = chartReady(chart);
  useEffect(() => {
    if (step === 'install' && ready) onChanged();
  }, [step, ready, onChanged]);

  const derived = defaultChartName(ref);
  const target = (nameInput.trim() || derived).toLowerCase();
  const clash = existing.find((a) => a.key === target);
  // Planning again is for an addon that is only planned. An installed one is
  // upgraded or reconfigured from its row: re-adding it would suspend it.
  const blocked = !!clash && !isPlanOnly(clash);
  const oci = ref.trim().toLowerCase().startsWith('oci://');

  async function create() {
    const parsed = parseValuesText(valuesText);
    if (parsed.error) {
      setErr(parsed.error);
      return;
    }
    if (blocked) return;
    if (!isChartRef(ref)) {
      setErr('the chart is an oci:// reference or an https:// link to a chart archive');
      return;
    }
    setBusy('create');
    setErr(null);
    try {
      const body: Record<string, unknown> = { chart: ref.trim() };
      if (version.trim()) body.version = version.trim();
      if (digest.trim()) body.digest = digest.trim();
      if (nameInput.trim()) body.name = nameInput.trim();
      if (parsed.values) body.values = parsed.values;
      const r = await api<{ name: string }>('/addon-charts', { method: 'POST', body: JSON.stringify(body) });
      setName(r.name);
      setValues(parsed.values ?? {});
      setStep('plan');
      onChanged();
    } catch (e) {
      if (e instanceof ApiError && (e.status === 404 || e.status === 405)) {
        onUnsupported('this portal-api cannot install addons from charts — update it first');
        return;
      }
      setErr(errText(e));
    } finally {
      setBusy(null);
    }
  }

  // change sends one completed edit and reads the addon again; the operator
  // then plans the new generation.
  function change(patch: Record<string, unknown>) {
    if (!name) return;
    setPending((n) => n + 1);
    setErr(null);
    queue.current = queue.current
      .then(async () => {
        await api(`/addon-charts/${encodeURIComponent(name)}`, { method: 'PATCH', body: JSON.stringify(patch) });
        await reload();
      })
      .catch((e) => setErr(errText(e)))
      .finally(() => setPending((n) => n - 1));
  }

  async function install() {
    if (!name) return;
    setBusy('install');
    setErr(null);
    try {
      await api(`/addon-charts/${encodeURIComponent(name)}/install`, { method: 'POST' });
      setStep('install');
      onChanged();
      void reload();
    } catch (e) {
      setErr(errText(e));
      void reload();
    } finally {
      setBusy(null);
    }
  }

  async function cancel() {
    if (!name) return;
    setBusy('cancel');
    setErr(null);
    try {
      await api(`/addon-charts/${encodeURIComponent(name)}`, { method: 'DELETE' });
      onChanged();
      onClose();
    } catch (e) {
      setErr(errText(e));
    } finally {
      setBusy(null);
    }
  }

  const blockers = installBlockers(chart);
  const neverApplied = !!chart && !chart.lastAppliedChart;
  const canInstall = step === 'plan' && !!chart && pending === 0 && planCurrent(chart) && blockers.length === 0;

  let footer;
  if (step === 'chart') {
    footer = (
      <>
        <Button variant="ghost" size="sm" onClick={onClose}>
          cancel
        </Button>
        <Button size="sm" leading={<ListChecks size={14} />} loading={busy === 'create'} disabled={!ref.trim() || blocked} onClick={create}>
          plan
        </Button>
      </>
    );
  } else if (step === 'plan') {
    footer = (
      <>
        {neverApplied && (
          <Button variant="ghost" size="sm" leading={<Trash2 size={13} />} loading={busy === 'cancel'} onClick={cancel}>
            cancel addon
          </Button>
        )}
        <Button variant="ghost" size="sm" onClick={onClose}>
          close
        </Button>
        <Button
          size="sm"
          leading={<Rocket size={14} />}
          loading={busy === 'install'}
          disabled={!canInstall}
          title={canInstall ? undefined : blockers.join('; ')}
          onClick={install}
        >
          install
        </Button>
      </>
    );
  } else {
    footer = (
      <Button size="sm" variant={ready ? undefined : 'ghost'} onClick={onClose}>
        {ready ? 'done' : 'close — it keeps installing'}
      </Button>
    );
  }

  return (
    <Modal open width={880} closeOnBackdrop={false} onClose={onClose} title={name ? `add addon ${name}` : 'add an addon from a chart'} footer={footer}>
      <div className="set__form">
        <div className="set__steps" aria-label="steps">
          {STEPS.map((s) => (
            <Badge key={s.step} tone={s.step === step ? 'blue' : 'neutral'}>
              {s.label}
            </Badge>
          ))}
        </div>

        {step === 'chart' && (
          <>
            <Text variant="muted" as="p">
              An addon is a Helm chart the operator installs next to the platform. Its workloads run non-root and
              unprivileged; nothing is installed before you have seen the plan.
            </Text>
            <Field label="chart" hint="an oci:// reference, or an https:// link to a chart archive (.tgz)" required>
              <Input
                value={ref}
                placeholder="oci://ghcr.io/example/charts/example"
                autoFocus
                onChange={(e) => setRef(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && create()}
              />
            </Field>
            <div className="set__row">
              <Field
                label="version"
                hint={oci || !ref.trim() ? 'required for an oci:// chart, unless the reference carries a tag' : 'an archive link is one version'}
              >
                <Input value={version} placeholder="1.2.0" disabled={!oci && !!ref.trim()} onChange={(e) => setVersion(e.target.value)} />
              </Field>
              <Field label="digest" hint="optional: pins the exact chart archive">
                <Input value={digest} placeholder="sha256:…" onChange={(e) => setDigest(e.target.value)} />
              </Field>
              <Field label="name" hint="the addon's key; defaults to the chart's name">
                <Input value={nameInput} placeholder={derived || 'example'} onChange={(e) => setNameInput(e.target.value)} />
              </Field>
            </div>
            {blocked && clash && (
              <span className="set__err">
                {clash.chart
                  ? `an addon named ${clash.key} is installed from a chart — upgrade it or change its values from its row, or choose another name.`
                  : `an addon named ${clash.key} is already installed from ${clash.proxyUrl || 'an address'} — remove it first, or choose another name.`}
              </span>
            )}
            {clash && !blocked && (
              <Text variant="dim">an addon named {clash.key} is planned already; planning this chart replaces that plan.</Text>
            )}
            <Field label="values" hint="optional, JSON — plain values; secret inputs are asked for with the plan, and any found here are offered to be moved">
              <Textarea rows={5} value={valuesText} placeholder={'{\n  "worker": { "replicas": 2 }\n}'} onChange={(e) => setValuesText(e.target.value)} />
            </Field>
          </>
        )}

        {step !== 'chart' && !chart && !loadErr && <Text variant="dim">reading {name}…</Text>}
        {loadErr && <span className="set__err">{loadErr}</span>}

        {step === 'plan' && chart && (
          <>
            <ChartPlan chart={chart} />
            <ChartValues
              chart={chart}
              values={values ?? {}}
              secretKeys={chart.secretKeys ?? []}
              disabled={busy !== null}
              onValues={(next) => {
                setValues(next);
                change({ values: next });
              }}
              onSecret={(path, value) => change({ secretValues: { [path]: value } })}
              onClearSecret={(path) => change({ clearSecrets: [path] })}
              onMoveSecrets={(secrets, next) => {
                setValues(next);
                change({ values: next, secretValues: secrets });
              }}
            />
            {pending > 0 && <Text variant="dim">saving the change…</Text>}
          </>
        )}

        {step === 'install' && chart && (
          <ChartProgress chart={chart}>
            {ready ? (
              <span className="set__ok set__done">
                <CircleCheck size={14} /> {chart.name} is installed and registered — its tiles and settings are in place.
              </span>
            ) : (
              <Text variant="dim">installing — the operator applies the chart; the portal registers the addon once it is ready.</Text>
            )}
          </ChartProgress>
        )}

        {err && <span className="set__err">{err}</span>}
      </div>
    </Modal>
  );
}
