import { useEffect, useState } from 'react';
import { TileGroup, Tile, Heading, Text, Spinner } from '@nalet/design-system';
import type { TileBadgeTone, TileStatus } from '@nalet/design-system';
import { Settings, Boxes, ScrollText, Radio, Database, LifeBuoy } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import { usePortalApi, type Launchpad as LaunchpadData } from './lib/api';
import { resolveIcon } from './lib/icons';
import { SetupCard } from './setup/SetupCard';
import './app.css';

// The launchpad home space — rendered inside the portal shell (which owns the
// header/sign-out). It is assembled at runtime from the portal-api registry:
// spaces → tiles → apps. Tiles launch products/apps (siblings on this origin via
// SSO, a full-page nav) or open external tools in a new tab. Admins additionally
// see a "settings" tile that opens the in-shell registry console.
export function Launchpad({
  isAdmin,
  adminElsewhere,
}: {
  isAdmin: boolean;
  // adminElsewhere: the caller holds the admin role, in a token issued to a
  // client the admin routes do not take — so no admin tile is offered.
  adminElsewhere?: { role: string; client: string } | null;
}) {
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

      {/* The first-run checklist, above the tiles, for an admin until one
          marks setup done. It renders nothing for anyone else. */}
      <SetupCard isAdmin={isAdmin} />

      {err && <p style={{ color: 'var(--signal-red, #F85149)' }}>couldn’t load your launchpad: {err}</p>}
      {!lp && !err && (
        <div className="lp__state">
          <Spinner /> <Text variant="muted">loading your launchpad…</Text>
        </div>
      )}

      {lp?.spaces.map((space) => (
        // columns="auto" (the default) = a responsive auto-fill grid, so the
        // launchpad collapses cleanly on phones instead of squeezing 3 columns.
        <TileGroup key={space.key} legend={space.title} gap="md">
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
              // the tile's open mode (settings-editable): newtab renders a
              // target=_blank link with the external glyph; inline navigates here.
              external={t.open === 'newtab' || undefined}
            />
          ))}
        </TileGroup>
      ))}

      {isAdmin && (
        <TileGroup legend="settings" gap="md">
          <Tile
            variant="app"
            title="registry"
            description="register apps, spaces & tiles"
            icon={Settings}
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/settings')}
          />
          <Tile
            variant="app"
            title="operator"
            description="instances · scale · update"
            icon={Boxes}
            status="online"
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/operator')}
          />
          <Tile
            variant="app"
            title="logs"
            description="container logs · redacted"
            icon={ScrollText}
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/logs')}
          />
          <Tile
            variant="app"
            title="events"
            description="kafka topology · live tail"
            icon={Radio}
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/events')}
          />
          <Tile
            variant="app"
            title="database"
            description="curated tables · read-only"
            icon={Database}
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/database')}
          />
          <Tile
            variant="app"
            title="export"
            description="support bundle · redacted"
            icon={LifeBuoy}
            badge="admin"
            badgeTone="info"
            onClick={() => nav('/export')}
          />
        </TileGroup>
      )}

      {adminElsewhere && (
        <Text variant="dim" as="p">
          you hold the {adminElsewhere.role} role, but signed in through {adminElsewhere.client ? `the client ${adminElsewhere.client}` : 'a client'}, which
          the portal does not take for admin requests — so its consoles stay closed. An operator names the portal&apos;s own clients in
          PORTAL_ADMIN_CLIENTS.
        </Text>
      )}

      <footer className="lp__foot">
        <Text variant="dim">zaentrum · demo</Text>
      </footer>
    </div>
  );
}
