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
  alwaysPull: boolean;
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

// usePortalApi returns a fetcher bound to the current access token. It throws an
// Error (with the server message) on any non-2xx.
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
      if (!res.ok) throw new Error(text.trim() || `portal-api ${res.status}`);
      return (text ? JSON.parse(text) : undefined) as T;
    },
    [token],
  );
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
