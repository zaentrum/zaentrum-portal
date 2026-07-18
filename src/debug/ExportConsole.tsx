import { useState } from 'react';
import { Card, Button, Checkbox, Text, Heading, Badge } from '@nalet/design-system';
import { Download, ShieldCheck } from 'lucide-react';
import { usePortalApi, useMe } from '../lib/api';
import './export.css';

type SectionKey = 'logs' | 'instances' | 'kafka' | 'registry' | 'config' | 'client';

const SECTIONS: { key: SectionKey; label: string; hint: string }[] = [
  { key: 'logs', label: 'Container logs', hint: 'recent log lines from every pod — secrets redacted' },
  { key: 'instances', label: 'Deployment & operator state', hint: 'running instances, replicas, images, operator status' },
  { key: 'kafka', label: 'Kafka topology', hint: 'topics, partitions, consumer groups, observed activity' },
  { key: 'registry', label: 'Registry', hint: 'launchpad apps, spaces & tiles' },
  { key: 'config', label: 'Runtime config', hint: 'non-secret settings (issuer, roles, brokers)' },
  { key: 'client', label: 'Browser / client state', hint: 'user agent, screen, identity roles, storage key names (never values)' },
];

// The support-bundle export: assembles a single JSON diagnostic file from the
// sections the operator opts into. Everything server-side is secret-scrubbed
// twice; the browser section carries only storage KEY names, never token values.
// Admin-only (RequireAdmin + the API gate).
export function ExportConsole() {
  const api = usePortalApi();
  const me = useMe();

  const [sel, setSel] = useState<Record<SectionKey, boolean>>({
    logs: true,
    instances: true,
    kafka: true,
    registry: true,
    config: true,
    client: true,
  });
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);

  function toggle(k: SectionKey) {
    setSel((s) => ({ ...s, [k]: !s[k] }));
    setDone(null);
  }

  // Browser state — deliberately excludes storage VALUES (OIDC tokens live there).
  function clientState() {
    const keys = (s: Storage) => {
      try {
        return Object.keys(s);
      } catch {
        return [];
      }
    };
    return {
      collectedAt: new Date().toISOString(),
      url: window.location.href,
      userAgent: navigator.userAgent,
      language: navigator.language,
      platform: navigator.platform,
      timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      screen: { width: window.screen.width, height: window.screen.height, dpr: window.devicePixelRatio },
      viewport: { width: window.innerWidth, height: window.innerHeight },
      online: navigator.onLine,
      identity: { username: me?.username ?? '', roles: me?.roles ?? [] },
      localStorageKeys: keys(window.localStorage),
      sessionStorageKeys: keys(window.sessionStorage),
    };
  }

  async function download() {
    setBusy(true);
    setErr(null);
    setDone(null);
    try {
      const flags = new URLSearchParams();
      (['logs', 'instances', 'kafka', 'registry', 'config'] as const).forEach((k) =>
        flags.set(k, sel[k] ? '1' : '0'),
      );
      const bundle = await api<Record<string, unknown>>(`/debug/support-bundle?${flags.toString()}`);
      if (sel.client) {
        const sections = (bundle.sections as Record<string, unknown>) ?? {};
        sections.client = clientState();
        bundle.sections = sections;
      }
      const blob = new Blob([JSON.stringify(bundle, null, 2)], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      const stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
      const name = `zaentrum-support-bundle-${stamp}.json`;
      a.href = url;
      a.download = name;
      a.click();
      URL.revokeObjectURL(url);
      setDone(name);
    } catch (e) {
      setErr(String((e as Error).message ?? e));
    } finally {
      setBusy(false);
    }
  }

  const anySelected = Object.values(sel).some(Boolean);

  return (
    <div className="exp">
      <div className="exp__head">
        <Heading level={1} chevron>
          export
        </Heading>
        <Text variant="muted">support bundle · secrets removed</Text>
      </div>

      <Card>
        <div className="exp__privacy">
          <ShieldCheck size={18} />
          <div>
            <Text variant="ui">passwords, tokens, API keys & database credentials are removed automatically</Text>
            <Text variant="dim">
              Every section is scrubbed server-side (twice). The browser section includes storage key names only —
              never their values. Uncheck anything you'd rather not share before downloading.
            </Text>
          </div>
        </div>

        <div className="exp__sections">
          {SECTIONS.map((s) => (
            <label className="exp__row" key={s.key}>
              <Checkbox checked={sel[s.key]} onChange={() => toggle(s.key)} />
              <div className="exp__rowtext">
                <Text variant="ui">{s.label}</Text>
                <Text variant="dim">{s.hint}</Text>
              </div>
            </label>
          ))}
        </div>

        <div className="exp__foot">
          <Button variant="primary" onClick={download} loading={busy} disabled={!anySelected}>
            <Download size={15} /> download bundle
          </Button>
          {done && <Badge tone="green">saved {done}</Badge>}
          {err && (
            <Text variant="muted" className="exp__err">
              {err}
            </Text>
          )}
        </div>
      </Card>
    </div>
  );
}
