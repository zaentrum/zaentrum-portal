import { useEffect, useState } from 'react';
import { TileGroup, Tile, Heading, Text, Spinner } from '@nalet/design-system';
import type { TileBadgeTone, TileStatus } from '@nalet/design-system';
import { Settings } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import { usePortalApi, type Launchpad as LaunchpadData } from './lib/api';
import { resolveIcon } from './lib/icons';
import './app.css';

// The launchpad home space — rendered inside the portal shell (which owns the
// header/sign-out). It is assembled at runtime from the portal-api registry:
// spaces → tiles → apps. Tiles launch products/apps (siblings on this origin via
// SSO, a full-page nav) or open external tools in a new tab. Admins additionally
// see a "settings" tile that opens the in-shell registry console.
export function Launchpad({ isAdmin }: { isAdmin: boolean }) {
  const api = usePortalApi();
  const nav = useNavigate();
  const [lp, setLp] = useState<LaunchpadData | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api<LaunchpadData>('/launchpad')
      .then((d) => live && setLp(d))
      .catch((e) => live && setErr(e instanceof Error ? e.message : String(e)));
    return () => {
      live = false;
    };
  }, [api]);

  return (
    <div className="lp">
      <div className="lp__head">
        <Heading level={2} chevron>
          welcome back
        </Heading>
        <Text variant="muted">your spaces and apps</Text>
      </div>

      {err && <p style={{ color: 'var(--danger, #f85149)' }}>couldn’t load your launchpad: {err}</p>}
      {!lp && !err && (
        <div className="lp__state">
          <Spinner /> <Text variant="muted">loading your launchpad…</Text>
        </div>
      )}

      {lp?.spaces.map((space) => (
        <TileGroup key={space.key} legend={space.title} columns={3} gap="md">
          {space.tiles.map((t) => (
            <Tile
              key={t.key}
              variant="app"
              title={t.title}
              description={t.description || undefined}
              icon={resolveIcon(t.icon)}
              status={(t.status || undefined) as TileStatus | undefined}
              badge={t.badge || undefined}
              badgeTone={(t.badgeTone || undefined) as TileBadgeTone | undefined}
              disabled={t.disabled || undefined}
              href={t.href || undefined}
              external={t.external || undefined}
            />
          ))}
        </TileGroup>
      ))}

      {isAdmin && (
        <TileGroup legend="system" columns={3} gap="md">
          <Tile
            variant="app"
            title="settings"
            description="register apps, spaces & tiles"
            icon={Settings}
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/settings')}
          />
        </TileGroup>
      )}

      <footer className="lp__foot">
        <Text variant="dim">zaentrum · demo</Text>
      </footer>
    </div>
  );
}
