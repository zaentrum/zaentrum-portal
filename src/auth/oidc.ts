import type { AuthProviderProps } from 'react-oidc-context';
import { WebStorageStateStore } from 'oidc-client-ts';

const env = import.meta.env;

// The portal is its OWN OIDC client (zaentrum-web on the bundled realm) — NOT
// chino's. It borrows the ISSUER from the server's /api/config so a single build
// is relocatable, and since a SHARED realm registers per-instance clients, it
// also adopts `oidcClientId.portal` from the same payload when present; the
// products (chino/tv/musig) ride the same Keycloak SSO session on this origin.
export let authority: string =
  env.VITE_OIDC_AUTHORITY ?? 'https://zaentrum.demo.nalet.cloud/auth/realms/zaentrum';
export let clientId: string = env.VITE_OIDC_CLIENT_ID ?? 'zaentrum-web';

// redirect_uri stays under /portal/ so it never collides with Keycloak's /auth route
// (and the /portal route already serves this SPA).
function buildConfig(): AuthProviderProps {
  return {
    authority,
    client_id: clientId,
    redirect_uri: `${window.location.origin}/portal/auth/callback`,
    post_logout_redirect_uri: `${window.location.origin}/portal/`,
    response_type: 'code',
    scope: 'openid profile email',
    automaticSilentRenew: true,
    userStore: new WebStorageStateStore({ store: window.localStorage }),
    onSigninCallback: () => {
      window.history.replaceState(
        null,
        '',
        window.location.pathname.replace(/\/auth\/callback$/, '/') || '/portal/',
      );
    },
  };
}

// Adopt the serving server's issuer (+ portal client id, when advertised) from
// GET /api/config (self-host discovery). Any failure keeps the build-time
// fallbacks.
export async function initAuth(): Promise<AuthProviderProps> {
  try {
    const res = await fetch('/api/config', { headers: { Accept: 'application/json' } });
    if (res.ok) {
      const cfg: unknown = await res.json();
      const issuer = (cfg as { oidcIssuer?: unknown }).oidcIssuer;
      if (typeof issuer === 'string' && issuer) authority = issuer;
      const ids = (cfg as { oidcClientId?: { portal?: unknown } }).oidcClientId;
      if (ids && typeof ids.portal === 'string' && ids.portal) clientId = ids.portal;
    }
  } catch {
    /* keep fallback authority */
  }
  return buildConfig();
}
