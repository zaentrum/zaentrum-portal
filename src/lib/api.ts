import { useCallback, useEffect, useState } from 'react';
import { useAuth } from 'react-oidc-context';

// The portal-api (the launchpad registry) is published under /api/portal behind
// the demo's path-routing. Override with VITE_PORTAL_API.
export const PORTAL_API: string =
  (import.meta.env.VITE_PORTAL_API as string | undefined) ?? '/api/portal';

// ─── registry types (mirror server/internal/model) ──────────────────────────

export interface LaunchTile {
  key: string;
  title: string;
  description: string;
  icon: string;
  href: string;
  order: number;
  badge: string;
  badgeTone: string;
  status: string;
  external: boolean;
  open: string; // inline|newtab (resolved server-side)
  disabled: boolean;
}
export interface LaunchSpace {
  key: string;
  title: string;
  order: number;
  tiles: LaunchTile[];
}
export interface Launchpad {
  spaces: LaunchSpace[];
}

export interface App {
  key: string;
  title: string;
  description: string;
  baseUrl: string;
  kind: string;
  healthUrl: string;
  icon: string;
  enabled: boolean;
  // The in-cluster address the shell proxies to when hosting this app inside
  // the portal. The server has always stored and returned it; the client type
  // omitted it, so the registry console could not set it and every app an admin
  // created was permanently non-embeddable — while the error told them to set a
  // field the form did not have.
  proxyUrl: string;
}
export interface Space {
  key: string;
  title: string;
  order: number;
}
export interface Tile {
  key: string;
  appKey: string;
  spaceKey: string;
  title: string;
  description: string;
  icon: string;
  target: string;
  order: number;
  badge: string;
  badgeTone: string;
  status: string;
  external: boolean;
  open: string; // inline|newtab|'' (unset -> external decides)
  enabled: boolean;
}
// Extension: an addon-contributed UI element for a named slot in a product app.
export interface Extension {
  key: string;
  addon: string;
  slot: string;
  kind: string; // link|action
  label: string;
  icon: string;
  url: string;
  method: string; // for kind=action
  statusUrl: string;
  ord: number;
  enabled: boolean;
}
export interface Me {
  username: string;
  roles: string[];
  isAdmin: boolean;
}

// ─── operator / instances ────────────────────────────────────────────────────

export interface Instance {
  name: string;
  image: string;
  desiredReplicas: number;
  readyReplicas: number;
  updatedReplicas: number;
  availableReplicas: number;
  restarts: number;
  phase: string; // ready|progressing|degraded|stopped
  protected: boolean;
  operatorManaged: boolean;
  // platform | addon | other — see groupOf() in server/internal/operator.
  group: string;
  // Why it is unhealthy, in the cluster's own words (ImagePullBackOff,
  // CrashLoopBackOff, OOMKilled …). Empty when it is fine.
  reason: string;
  alwaysPull: boolean;
  // The zaentrum.io/addon and zaentrum.io/component labels, when the addon's
  // deployment channel stamped them. Grouping metadata only.
  addon?: string;
  component?: string;
}
export interface OperatorComponent {
  name: string;
  ready: boolean;
  image: string;
}
// When present is false (the demo / no operator) the backend omits the rest and
// may include a note, so the detail fields are optional.
export interface OperatorInfo {
  present: boolean;
  note?: string;
  name?: string;
  channel?: string;
  version?: string;
  updateMode?: string;
  hostname?: string;
  phase?: string;
  currentVersion?: string;
  availableUpdate?: string;
  components?: OperatorComponent[];
}
export interface OperatorState {
  available: boolean;
  operator: OperatorInfo;
  instances: Instance[];
  error?: string;
}

// ─── addons (mirror server/internal/api/addons.go) ───────────────────────────

// AddonComponent is one declared workload with its live state. The state
// fields are null when the workload is not deployed; phase is "unknown" (the
// rest null) when the portal cannot see workloads at all.
export interface AddonComponent {
  name: string;
  workload: string;
  role: string; // primary|required|optional
  summary: string;
  topics?: string[];
  phase: string | null;
  ready: number | null;
  desired: number | null;
  restarts: number | null;
  reason: string | null;
}
export interface AddonSetupSection {
  key: string;
  title: string;
  description?: string;
  required: boolean;
  target?: string;
  ord: number;
}
// AddonSetup is what the addon DECLARES: where its setup status is served
// (relative to the addon, reached through the app proxy) and the sections.
export interface AddonSetup {
  path: string;
  sections?: AddonSetupSection[];
}
export interface InstalledAddon {
  key: string;
  title: string;
  proxyUrl: string;
  version: string;
  installedAt: string;
  refreshedAt: string;
  tiles: number;
  slots: number;
  components: AddonComponent[];
  setup: AddonSetup | null;
  refreshAvailable: boolean;
  // chart: the Helm chart the operator installs the addon from; null for an
  // addon added by its address. phase and suspended are its ZaentrumAddon's.
  chart: AddonChartInfo | null;
  phase: string;
  suspended: boolean;
  // registered: the registry holds the addon. A chart addon the operator has
  // not made ready yet is listed with nothing registered.
  registered: boolean;
  registrationError: string;
}

// ─── addons as Helm charts (mirror server/internal/api/addoncharts.go) ───────

export interface ChartSource {
  ref: string;
  version?: string;
  digest?: string;
}
export interface AddonChartInfo extends ChartSource {
  // lastApplied: the chart running; null while nothing was applied.
  lastApplied: ChartSource | null;
}
export interface ChartWorkload {
  kind: string;
  name: string;
  images: string[] | null;
  ports: unknown[] | null;
}
// ChartPlan is what the operator reports for the addon's spec: the chart as
// rendered, what it would apply and what it refuses. Nothing of it is applied
// while the addon is suspended.
export interface ChartPlan {
  chart: {
    name: string;
    version: string;
    appVersion?: string;
    description?: string;
    digest?: string;
    annotations?: Record<string, string> | null;
  };
  valuesSchema: string;
  valuesErrors: string[] | null;
  violations: string[] | null;
  objects: { kind: string; name: string }[] | null;
  workloads: ChartWorkload[] | null;
  changes?: { added?: string[] | null; removed?: string[] | null; images?: string[] | null } | null;
}
export interface ChartComponentStatus {
  name: string;
  kind: string;
  ready: number;
  desired: number;
  reason?: string;
}
// AddonChart is GET /addon-charts/{name}. No secret value: secretKeys names
// the secret inputs that are set.
export interface AddonChart {
  name: string;
  chart: ChartSource;
  suspended: boolean;
  phase: string;
  message: string;
  generation: number;
  observedGeneration: number;
  plan: ChartPlan | null;
  components: ChartComponentStatus[];
  lastAppliedChart: ChartSource | null;
  values: Record<string, unknown> | null;
  secretKeys: string[];
  registered: boolean;
  registrationError?: string;
}
export interface AddonChartsStatus {
  available: boolean;
  note: string;
}
// InstallResult answers both a dry run ("check") and a real install.
export interface InstallResult {
  key: string;
  app: App;
  space: Space | null;
  tiles: number;
  slots: number;
  commands: number;
  checks: number;
  version: string;
  components: AddonComponent[];
  setup: AddonSetup | null;
  refresh: boolean;
  // adopt: an app of this key exists at this address without an addon record
  // (installed before component groups, with no tile or slot row); installing
  // records it as the addon.
  adopt?: boolean;
  // previousAddress: the addon is installed from another address; installing
  // from this one moves it and must be confirmed with replaceAddress.
  previousAddress?: string;
  dryRun: boolean;
}
export interface RemoveResult {
  removed: { tiles: number; rows: number; space: string };
  remainingWorkloads: string[];
}
// SetupStatus is what the ADDON answers at its setup path. Addon-authored, so
// the console treats every field as untrusted plain text.
export type SetupState = 'ready' | 'needs-setup' | 'degraded' | 'unknown';
export interface SetupStatus {
  state: SetupState;
  sections: { key: string; state: SetupState; summary: string }[];
}

// ApiError is a non-2xx answer: the server's message, and the status a caller
// may branch on (404 from an older portal-api, 409 from a refused install).
export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

// usePortalApi returns a fetcher bound to the current access token. It throws an
// ApiError (with the server message) on any non-2xx.
export function usePortalApi() {
  const auth = useAuth();
  const token = auth.user?.access_token;
  return useCallback(
    async function api<T>(path: string, init?: RequestInit): Promise<T> {
      const res = await fetch(`${PORTAL_API}${path}`, {
        ...init,
        headers: {
          Accept: 'application/json',
          ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          ...(init?.headers ?? {}),
        },
      });
      const text = await res.text();
      if (!res.ok) throw new ApiError(res.status, text.trim() || `portal-api ${res.status}`);
      return (text ? JSON.parse(text) : undefined) as T;
    },
    [token],
  );
}

// usePortalText fetches a text/plain endpoint (container logs) with auth and
// returns the raw body — usePortalApi always JSON-parses, which logs are not.
export function usePortalText() {
  const auth = useAuth();
  const token = auth.user?.access_token;
  return useCallback(
    async function text(path: string, init?: RequestInit): Promise<string> {
      const res = await fetch(`${PORTAL_API}${path}`, {
        ...init,
        headers: {
          Accept: 'text/plain',
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
          ...(init?.headers ?? {}),
        },
      });
      const body = await res.text();
      if (!res.ok) throw new Error(body.trim() || `portal-api ${res.status}`);
      return body;
    },
    [token],
  );
}

// DebugPod is one pod + its container names for the log-viewer selector.
export interface DebugPod {
  pod: string;
  phase: string;
  containers: string[];
}

// ─── kafka event tap (mirror server/internal/eventtap) ───────────────────────

export interface KafkaTopic {
  topic: string;
  partitions: number;
  consumers: string[];
  seen: number;
  lastEvent?: string;
}
export interface KafkaTopology {
  available: boolean;
  brokers?: string[];
  topics?: KafkaTopic[];
  groups?: string[];
  note?: string;
}
export interface KafkaEvent {
  seq: number;
  topic: string;
  partition: number;
  offset: number;
  key: string;
  time: string;
  type?: string;
  itemId?: string;
  payload: string;
  size: number;
}

// ─── curated db browser (mirror server/internal/dbbrowse) ────────────────────

export interface DbTable {
  key: string;
  db: string;
  label: string;
  description: string;
  rows: number;
}
export interface DbPage {
  table: string;
  db: string;
  columns: string[];
  rows: string[][];
  total: number;
  limit: number;
  offset: number;
}

// useMe resolves the caller's identity + admin flag from the portal-api.
export function useMe(): Me | null {
  const api = usePortalApi();
  const [me, setMe] = useState<Me | null>(null);
  useEffect(() => {
    let live = true;
    api<Me>('/me')
      .then((m) => live && setMe(m))
      .catch(() => live && setMe({ username: '', roles: [], isAdmin: false }));
    return () => {
      live = false;
    };
  }, [api]);
  return me;
}
