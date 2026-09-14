import type { BadgeTone } from '@nalet/design-system';
import type { AddonComponent, InstalledAddon, SetupState, SetupStatus } from './api';

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
  };
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
