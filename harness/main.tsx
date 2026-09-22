import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { MemoryRouter } from 'react-router-dom';
import '@nalet/design-system/styles.css';
import '../src/app.css';
import { OperatorConsole } from '../src/operator/OperatorConsole';
import { SettingsConsole } from '../src/settings/SettingsConsole';

// ?view=settings renders the registry console instead. Any portal view can be
// added here — the point is that each one becomes viewable without a cluster.
const params = new URLSearchParams(location.search);
const view = params.get('view');
const View = view === 'settings' ? SettingsConsole : OperatorConsole;

// ?charts=off plays an older portal-api without chart addons, ?charts=nocrd a
// cluster without the ZaentrumAddon resource. The mock server reads it from a
// cookie, which the dev proxy passes on.
document.cookie = `mock-charts=${params.get('charts') ?? ''}; path=/; SameSite=Lax`;
// ?controller=olm|manifest|appliance|unknown|none picks how the operator
// reports its own controller — `none` being every operator older than the
// field, which the console has to render too.
document.cookie = `mock-controller=${params.get('controller') ?? ''}; path=/; SameSite=Lax`;

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    {/* The settings console links into addon consoles, so it needs a router. */}
    <MemoryRouter>
      <div style={{ padding: 24, maxWidth: 1200, margin: '0 auto' }}>
        <View />
      </div>
    </MemoryRouter>
  </StrictMode>,
);
