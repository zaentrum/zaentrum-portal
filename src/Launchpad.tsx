import { TileGroup, Tile, Heading, Text } from '@nalet/design-system';
import { Library } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import { CGlyph, TGlyph, MGlyph } from './glyphs';
import './app.css';

// products are siblings on this origin (SSO); the tile full-page-navigates there.
// trailing slash so it lands on chino's /chino/ base directly.
const CHINO_URL = '/chino/';

// The launchpad home space — rendered inside the portal shell (which owns the
// header/sign-out). Tiles launch products (siblings via SSO) and in-shell apps
// (the katalog console routes internally).
export function Launchpad() {
  const nav = useNavigate();

  return (
    <div className="lp">
      <div className="lp__head">
        <Heading level={2} chevron>
          welcome back
        </Heading>
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

      <TileGroup legend="manage" columns={3} gap="md">
        <Tile
          variant="app"
          title="katalog"
          description="catalog management"
          icon={Library}
          status="online"
          badge="admin"
          badgeTone="info"
          onClick={() => nav('/katalog')}
        />
      </TileGroup>

      <footer className="lp__foot">
        <Text variant="dim">zaentrum · demo</Text>
      </footer>
    </div>
  );
}
