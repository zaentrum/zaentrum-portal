import type { BadgeTone } from '@nalet/design-system';
import type { AddonChart, AddonComponent, ChartComponentStatus, InstalledAddon, SetupState, SetupStatus } from './api';

// The SPA ships in its own image, so during a rollout it can run against an
// older portal-api. That one answers GET /addons with 200 and rows of
// {key, title, proxyUrl, tiles, slots} — no version, components or setup — and
// ignores dryRun on POST /addons, installing instead of checking.

// hasComponentGroups answers whether an /addons answer comes from a portal-api
// that knows component groups.
export function hasComponentGroups(rows: unknown): rows is InstalledAddon[] {
  return Array.isArray(rows) && rows.every((r) => !!r && Array.isArray((r as { components?: unknown }).components));
}

// withAddonDefaults fills what an older portal-api leaves out, so a row of
// either shape renders: no containers, no setup, nothing to refresh.
export function withAddonDefaults(a: Partial<InstalledAddon> & { key: string }): InstalledAddon {
  return {
    ...a,
    title: a.title ?? '',
    proxyUrl: a.proxyUrl ?? '',
    version: a.version ?? '',
    installedAt: a.installedAt ?? '',
    refreshedAt: a.refreshedAt ?? '',
    tiles: a.tiles ?? 0,
    slots: a.slots ?? 0,
    components: Array.isArray(a.components) ? a.components : [],
    setup: a.setup ?? null,
    refreshAvailable: a.refreshAvailable ?? false,
    // A portal-api without chart addons lists only what the registry holds.
    chart: a.chart ?? null,
    phase: a.phase ?? '',
    suspended: a.suspended ?? false,
    registered: a.registered ?? true,
    registrationError: a.registrationError ?? '',
  };
}

// ─── chart addons ────────────────────────────────────────────────────────────

// chartPhaseTone colours a ZaentrumAddon phase.
export function chartPhaseTone(phase: string | undefined): BadgeTone {
  switch (phase) {
    case 'Ready':
      return 'green';
    case 'Pending':
    case 'Planned':
    case 'Installing':
      return 'blue';
    case 'PlanFailed':
    case 'Failed':
    case 'Degraded':
      return 'amber';
    default:
      return 'neutral';
  }
}

// phaseLabel is a ZaentrumAddon phase as the console words it: "PlanFailed"
// reads "plan failed"; no phase yet reads "pending".
export function phaseLabel(phase: string | undefined): string {
  if (!phase) return 'pending';
  return phase.replace(/([a-z])([A-Z])/g, '$1 $2').toLowerCase();
}

// isPlanOnly: a chart addon that was planned and never applied. Cancelling it
// removes nothing that runs.
export function isPlanOnly(a: InstalledAddon): boolean {
  return !!a.chart && a.suspended && !a.chart.lastApplied && !a.registered;
}

// upgradePlanned: an applied chart addon whose spec names another chart than
// the one running, planned and waiting to be applied.
export function upgradePlanned(a: InstalledAddon): boolean {
  const c = a.chart;
  return !!c && !!c.lastApplied && a.suspended && (c.lastApplied.ref !== c.ref || (c.lastApplied.version ?? '') !== (c.version ?? ''));
}

// planCurrent: the operator has planned the addon's current generation, so
// the plan shown is the plan an install applies.
export function planCurrent(c: AddonChart | null): boolean {
  return !!c && !!c.plan && c.observedGeneration === c.generation;
}

// installBlockers is why installing is refused right now; [] when it is not.
// Mirrors installable() in server/internal/api/addoncharts.go.
export function installBlockers(c: AddonChart | null): string[] {
  if (!c || !planCurrent(c)) return ['the operator has not planned the current configuration yet'];
  return [...(c.plan?.violations ?? []), ...(c.plan?.valuesErrors ?? [])];
}

// chartReady: installed and registered — what the wizard waits for.
export function chartReady(c: AddonChart | null): boolean {
  return !!c && !c.suspended && c.phase === 'Ready' && c.registered;
}

// componentsProgress sums a chart addon's workloads: "1/2 ready".
export function componentsProgress(components: ChartComponentStatus[] | null | undefined): string {
  const cs = components ?? [];
  if (cs.length === 0) return 'no workloads reported yet';
  const ready = cs.filter((c) => c.desired > 0 && c.ready >= c.desired).length;
  return `${ready}/${cs.length} ready`;
}

// portLabel renders a workload port in whatever shape the operator reports it:
// a number, a name, or a container port object.
export function portLabel(p: unknown): string {
  if (typeof p === 'number' || typeof p === 'string') return String(p);
  if (p && typeof p === 'object') {
    const o = p as Record<string, unknown>;
    const port = o.containerPort ?? o.port;
    if (typeof port === 'number' || typeof port === 'string') {
      const name = typeof o.name === 'string' && o.name ? `${o.name} ` : '';
      const proto = typeof o.protocol === 'string' && o.protocol && o.protocol !== 'TCP' ? `/${o.protocol.toLowerCase()}` : '';
      return `${name}${port}${proto}`;
    }
  }
  return JSON.stringify(p);
}

// defaultChartName mirrors defaultAddonName in server/internal/api/addoncharts.go:
// the last path segment of a chart reference without a tag, a version or an
// archive extension.
export function defaultChartName(ref: string): string {
  const withoutScheme = ref.trim().replace(/^[a-z][a-z0-9+.-]*:\/\//i, '');
  const path = withoutScheme.replace(/[?#].*$/, '').replace(/\/+$/, '');
  const slash = path.indexOf('/');
  if (slash < 0) return '';
  let last = path.slice(path.lastIndexOf('/') + 1);
  last = last.replace(/[:@].*$/, '');
  last = last.replace(/(\.tar\.gz|\.tgz)$/i, '');
  last = last.replace(/-v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]*)?$/, '');
  return last.toLowerCase();
}

// isChartRef: what the server accepts as a chart reference, roughly — the
// server has the final word.
export function isChartRef(ref: string): boolean {
  return /^(oci|https):\/\/[^\s/]+\/\S+$/i.test(ref.trim());
}

// joinTarget mirrors trimLeadingSlash in server/internal/api/addons.go: a
// target is a hash route, a query or a path INSIDE the addon's console, joined
// to /portal/app/<key> without doubling the separator.
export function joinTarget(target?: string): string {
  const t = (target ?? '').trim();
  if (!t || t.startsWith('#') || t.startsWith('?')) return t;
  return '/' + t.replace(/^\/+/, '');
}

// appRoute is the shell route (the router is based at /portal) that opens a
// place inside an installed addon's console.
export function appRoute(key: string, target?: string): string {
  return `/app/${encodeURIComponent(key)}${joinTarget(target)}`;
}

export function phaseTone(phase: string | null | undefined): BadgeTone {
  switch (phase) {
    case 'ready':
      return 'green';
    case 'progressing':
      return 'blue';
    case 'degraded':
      return 'amber';
    default:
      return 'neutral';
  }
}

// componentPhase is what a component's status cell says: the workload's phase,
// "not deployed" when nothing by that name runs, "unknown" when the portal
// cannot see workloads.
export function componentPhase(c: AddonComponent): string {
  return c.phase ?? 'not deployed';
}

// containersSummary is the one-line state of an addon's workloads, e.g.
// "2/2 running". A required component that is not running makes it amber; an
// optional one that is not deployed does not.
export function containersSummary(components: AddonComponent[] | undefined): { text: string; tone: BadgeTone } {
  const cs = components ?? [];
  if (cs.length === 0) return { text: '—', tone: 'neutral' };
  if (cs.some((c) => c.phase === 'unknown')) return { text: `${cs.length} declared · unknown`, tone: 'neutral' };
  const running = cs.filter((c) => c.phase === 'ready').length;
  if (running === cs.length) return { text: `${running}/${cs.length} running`, tone: 'green' };
  const requiredDown = cs.some((c) => c.role !== 'optional' && c.phase !== 'ready');
  return { text: `${running}/${cs.length} running`, tone: requiredDown ? 'amber' : 'blue' };
}

export function setupTone(state: SetupState): BadgeTone {
  switch (state) {
    case 'ready':
      return 'green';
    case 'needs-setup':
    case 'degraded':
      return 'amber';
    default:
      return 'neutral';
  }
}

const SETUP_STATES: readonly string[] = ['ready', 'needs-setup', 'degraded'];
const MAX_SUMMARY = 200;

export const UNKNOWN_SETUP: SetupStatus = { state: 'unknown', sections: [] };

const asState = (v: unknown): SetupState =>
  typeof v === 'string' && SETUP_STATES.includes(v) ? (v as SetupState) : 'unknown';

// parseSetupStatus accepts only the documented shape of an addon's setup
// answer. It is addon-authored: anything else becomes "unknown", and summaries
// are clipped plain text — the console renders them as text, never markup.
export function parseSetupStatus(raw: unknown): SetupStatus {
  if (!raw || typeof raw !== 'object') return UNKNOWN_SETUP;
  const r = raw as { state?: unknown; sections?: unknown };
  const sections = Array.isArray(r.sections)
    ? r.sections.flatMap((s: unknown) => {
        if (!s || typeof s !== 'object') return [];
        const sec = s as { key?: unknown; state?: unknown; summary?: unknown };
        if (typeof sec.key !== 'string') return [];
        const summary = typeof sec.summary === 'string' ? sec.summary.slice(0, MAX_SUMMARY) : '';
        return [{ key: sec.key, state: asState(sec.state), summary }];
      })
    : [];
  return { state: asState(r.state), sections };
}
