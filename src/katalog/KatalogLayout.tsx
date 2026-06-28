import { Tabs, Heading } from '@nalet/design-system';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import './katalog.css';

const SECTIONS = [
  { value: 'catalog', label: 'catalog', path: '/katalog' },
  { value: 'scan', label: 'scan', path: '/katalog/scan' },
  { value: 'downloads', label: 'downloads', path: '/katalog/downloads' },
  { value: 'settings', label: 'settings', path: '/katalog/settings' },
];

function sectionFor(pathname: string): string {
  if (pathname.startsWith('/katalog/scan')) return 'scan';
  if (pathname.startsWith('/katalog/downloads')) return 'downloads';
  if (pathname.startsWith('/katalog/settings')) return 'settings';
  return 'catalog';
}

// The katalog console: an app inside the portal shell. Its own section nav
// (catalog / scan / downloads / settings) drives a nested route outlet.
export function KatalogLayout() {
  const nav = useNavigate();
  const loc = useLocation();
  const active = sectionFor(loc.pathname);

  return (
    <div className="kat">
      <div className="kat__head">
        <Heading level={1} chevron>
          katalog
        </Heading>
        <span className="kat__sub">catalog management console</span>
      </div>
      <Tabs
        items={SECTIONS.map((s) => ({ value: s.value, label: s.label }))}
        value={active}
        onChange={(v) => {
          const s = SECTIONS.find((x) => x.value === v);
          if (s) nav(s.path);
        }}
      />
      <div className="kat__body">
        <Outlet />
      </div>
    </div>
  );
}
