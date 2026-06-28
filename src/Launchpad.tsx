import { TileGroup, Tile, Heading, Text, Badge, Button } from '@nalet/design-system';
import { LogOut } from 'lucide-react';
import type { AuthContextProps } from 'react-oidc-context';
import { ZaentrumLockup, CGlyph, TGlyph, MGlyph } from './glyphs';
import './app.css';

// products are siblings on this origin (SSO); the tile full-page-navigates there.
// trailing slash so it lands on chino's /chino/ base directly.
const CHINO_URL = '/chino/';

export function Launchpad({ auth }: { auth: AuthContextProps }) {
  const p = auth.user?.profile;
  const name = (p?.preferred_username as string) || (p?.name as string) || 'you';

  return (
    <div className="lp">
      <header className="lp__bar">
        <ZaentrumLockup height={26} />
        <div className="lp__bar-right">
          <Badge tone="blue" dot>{name}</Badge>
          <Button
            variant="ghost"
            size="sm"
            leading={<LogOut size={15} strokeWidth={1.75} />}
            onClick={() => void auth.signoutRedirect()}
          >
            sign out
          </Button>
        </div>
      </header>

      <main className="lp__main">
        <div className="lp__head">
          <Heading level={2} chevron>welcome back</Heading>
          <Text variant="muted">your spaces and apps</Text>
        </div>

        <TileGroup legend="apps" columns={3} gap="md">
          <Tile
            variant="app"
            title="chino"
            description="movies & shows"
            icon={CGlyph}
            status="online"
            badge="ready"
            badgeTone="success"
            href={CHINO_URL}
          />
          <Tile variant="app" title="tv" description="live channels" icon={TGlyph} status="offline" badge="soon" disabled />
          <Tile variant="app" title="musig" description="music" icon={MGlyph} status="offline" badge="soon" disabled />
        </TileGroup>
      </main>

      <footer className="lp__foot">
        <Text variant="dim">zaentrum · demo</Text>
      </footer>
    </div>
  );
}
