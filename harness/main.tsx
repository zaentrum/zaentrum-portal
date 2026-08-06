import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@nalet/design-system/styles.css';
import '../src/app.css';
import { OperatorConsole } from '../src/operator/OperatorConsole';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <div style={{ padding: 24, maxWidth: 1200, margin: '0 auto' }}>
      <OperatorConsole />
    </div>
  </StrictMode>,
);
