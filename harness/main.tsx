import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import '@nalet/design-system/styles.css';
import '../src/app.css';
import { OperatorConsole } from '../src/operator/OperatorConsole';
import { SettingsConsole } from '../src/settings/SettingsConsole';
import { Launchpad } from '../src/Launchpad';
import { PeoplePage } from '../src/people/PeoplePage';
import { InvitePage } from '../src/invite/InvitePage';
import { Shell } from '../src/shell/Shell';

// ?view=settings renders the registry console instead, ?view=launchpad the
// launchpad with its setup checklist, ?view=people the People page,
// ?view=shell the launchpad inside the shell — its header, the notices bell —
// ?view=invite&token=… the invite page a link opens (without the shell: it
// needs no sign-in). Any portal view can be added here — the point is that
// each one becomes viewable without a cluster.
const params = new URLSearchParams(location.search);
const view = params.get('view');
const InShell = () => (
  <Routes>
    <Route element={<Shell />}>
      <Route index element={<Launchpad isAdmin />} />
      <Route path="*" element={<p style={{ padding: 24 }}>a page inside the shell: a notice's link opened it</p>} />
    </Route>
  </Routes>
);
const View =
  view === 'settings'
    ? SettingsConsole
    : view === 'launchpad'
      ? () => <Launchpad isAdmin />
      : view === 'people'
        ? PeoplePage
        : view === 'shell'
          ? InShell
          : OperatorConsole;

// ?charts=off plays an older portal-api without chart addons, ?charts=nocrd a
// cluster without the ZaentrumAddon resource. The mock server reads it from a
// cookie, which the dev proxy passes on.
document.cookie = `mock-charts=${params.get('charts') ?? ''}; path=/; SameSite=Lax`;
// ?controller=olm|manifest|appliance|unknown|none picks how the operator
// reports its own controller — `none` being every operator older than the
// field, which the console has to render too.
document.cookie = `mock-controller=${params.get('controller') ?? ''}; path=/; SameSite=Lax`;
// ?verification=passed|running|requested|never|off|unreadable|old picks the
// record the platform's verification starts from (failed by default) — `old`
// being a portal-api older than the field, which sends none.
document.cookie = `mock-verification=${params.get('verification') ?? ''}; path=/; SameSite=Lax`;
// ?setup=filled|scanning|https|ready|done|unknown|old picks where first-run
// setup starts (a fresh appliance by default); ?gpu=1 gives the cluster a GPU
// node.
document.cookie = `mock-setup=${params.get('setup') ?? ''}; path=/; SameSite=Lax`;
document.cookie = `mock-gpu=${params.get('gpu') ?? ''}; path=/; SameSite=Lax`;
// ?people=external|unavailable: people in an identity provider, or a People
// page without its client's secret.
document.cookie = `mock-people=${params.get('people') ?? ''}; path=/; SameSite=Lax`;
// ?notices=none|old|down: no notices yet, a portal-api without notices (no
// bell), one that does not answer.
document.cookie = `mock-notices=${params.get('notices') ?? ''}; path=/; SameSite=Lax`;

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    {view === 'invite' ? (
      <MemoryRouter initialEntries={[`/invite/${params.get('token') ?? ''}`]}>
        <InvitePage />
      </MemoryRouter>
    ) : (
      // The settings console links into addon consoles, so it needs a router.
      <MemoryRouter>
        <div className="harness-page">
          <View />
        </div>
      </MemoryRouter>
    )}
  </StrictMode>,
);
