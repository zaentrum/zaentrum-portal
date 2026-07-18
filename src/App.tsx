import { useEffect } from 'react';
import type { ReactNode } from 'react';
import { useAuth } from 'react-oidc-context';
import { Navigate, Route, Routes } from 'react-router-dom';
import { Spinner, Text } from '@nalet/design-system';
import { Shell } from './shell/Shell';
import { Launchpad } from './Launchpad';
import { SettingsConsole } from './settings/SettingsConsole';
import { OperatorConsole } from './operator/OperatorConsole';
import { LogsConsole } from './debug/LogsConsole';
import { useMe } from './lib/api';
import { Splash } from './Splash';

// You sign into zaentrum (the portal) once; products + apps ride the same SSO
// session. Unauthenticated hits bounce to Keycloak.
export function App() {
  const auth = useAuth();

  useEffect(() => {
    if (!auth.isLoading && !auth.isAuthenticated && !auth.activeNavigator && !auth.error) {
      void auth.signinRedirect();
    }
  }, [auth.isLoading, auth.isAuthenticated, auth.activeNavigator, auth.error]);

  if (auth.error) return <Splash message={`sign-in failed: ${auth.error.message}`} />;
  if (!auth.isAuthenticated) return <Splash message="signing you in…" />;
  return <AuthedApp />;
}

// AuthedApp resolves the caller's identity (for admin gating) and mounts the
// launchpad shell. The launchpad is assembled from the portal-api registry; the
// katalog console is now its own service (a registry app at /katalog/), so it is
// no longer routed in-shell.
function AuthedApp() {
  const me = useMe();
  const isAdmin = !!me?.isAdmin;

  return (
    <Routes>
      <Route element={<Shell />}>
        <Route index element={<Launchpad isAdmin={isAdmin} />} />
        <Route
          path="settings/*"
          element={
            <RequireAdmin me={me}>
              <SettingsConsole />
            </RequireAdmin>
          }
        />
        <Route
          path="operator/*"
          element={
            <RequireAdmin me={me}>
              <OperatorConsole />
            </RequireAdmin>
          }
        />
        <Route
          path="logs/*"
          element={
            <RequireAdmin me={me}>
              <LogsConsole />
            </RequireAdmin>
          }
        />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}

// RequireAdmin waits for the identity to resolve, then admits admins or bounces
// everyone else home (settings is admin-only; the API also enforces this).
function RequireAdmin({ me, children }: { me: ReturnType<typeof useMe>; children: ReactNode }) {
  if (me === null) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '24px 0' }}>
        <Spinner /> <Text variant="muted">checking access…</Text>
      </div>
    );
  }
  if (!me.isAdmin) return <Navigate to="/" replace />;
  return <>{children}</>;
}
