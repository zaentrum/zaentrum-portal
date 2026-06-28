import { TileGroup, Tile, Heading, Text, Badge } from '@nalet/design-system';
import { ZaentrumLockup, CGlyph, TGlyph, MGlyph } from './glyphs';
import './app.css';

// chino is the one live product; the launchpad tile hands off to its app.
const CHINO_URL = 'https://zaentrum.demo.nalet.cloud/chino';

export function App() {
  return (
    <div className="lp">
      <header className="lp__bar">
        <ZaentrumLockup height={26} />
        <div className="lp__bar-right">
          <Badge tone="blue" dot>demo</Badge>
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
          <Tile
            variant="app"
            title="tv"
            description="live channels"
            icon={TGlyph}
            status="offline"
            badge="soon"
            disabled
          />
          <Tile
            variant="app"
            title="musig"
            description="music"
            icon={MGlyph}
            status="offline"
            badge="soon"
            disabled
          />
        </TileGroup>
      </main>

      <footer className="lp__foot">
        <Text variant="dim">zaentrum · demo</Text>
      </footer>
    </div>
  );
}
