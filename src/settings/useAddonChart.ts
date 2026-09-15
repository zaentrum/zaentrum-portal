import { useCallback, useEffect, useRef, useState } from 'react';
import { usePortalApi, type AddonChart } from '../lib/api';

// How often a chart addon is read while the operator plans or installs it.
export const CHART_POLL_MS = 1500;

export const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

// useAddonChart reads GET /addon-charts/{name}, and again every intervalMs
// while it is non-zero. A slow answer never replaces a newer one. name null
// reads nothing.
export function useAddonChart(name: string | null, intervalMs: number) {
  const api = usePortalApi();
  const [chart, setChart] = useState<AddonChart | null>(null);
  const [error, setError] = useState<string | null>(null);
  const sent = useRef(0);
  const applied = useRef(0);

  const reload = useCallback(async (): Promise<AddonChart | null> => {
    if (!name) return null;
    const n = ++sent.current;
    try {
      const c = await api<AddonChart>(`/addon-charts/${encodeURIComponent(name)}`);
      if (n > applied.current) {
        applied.current = n;
        setChart(c);
        setError(null);
      }
      return c;
    } catch (e) {
      if (n > applied.current) {
        applied.current = n;
        setError(errText(e));
      }
      return null;
    }
  }, [api, name]);

  useEffect(() => {
    if (!name) return;
    void reload();
    if (!intervalMs) return;
    const t = setInterval(() => void reload(), intervalMs);
    return () => clearInterval(t);
  }, [reload, name, intervalMs]);

  return { chart, error, reload };
}
