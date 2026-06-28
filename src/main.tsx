import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { AuthProvider } from 'react-oidc-context';
import '@nalet/design-system/styles.css';
import { App } from './App';
import { initAuth } from './auth/oidc';

// resolve the issuer from /api/config (self-host discovery) before mounting the
// AuthProvider, so the portal points at whatever Keycloak serves it.
void initAuth().then((oidcConfig) => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <AuthProvider {...oidcConfig}>
        <App />
      </AuthProvider>
    </StrictMode>,
  );
});
