import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@nalet/design-system/styles.css';
import '../src/app.css';
import { OperatorConsole } from '../src/operator/OperatorConsole';
import { SettingsConsole } from '../src/settings/SettingsConsole';

// ?view=settings renders the registry console instead. Any portal view can be
// added here — the point is that each one becomes viewable without a cluster.
const view = new URLSearchParams(location.search).get('view');
const View = view === 'settings' ? SettingsConsole : OperatorConsole;

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <div style={{ padding: 24, maxWidth: 1200, margin: '0 auto' }}>
      <View />
    </div>
  </StrictMode>,
);
