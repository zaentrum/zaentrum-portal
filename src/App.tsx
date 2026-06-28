import { useEffect } from 'react';
import { useAuth } from 'react-oidc-context';
import { Navigate, Route, Routes } from 'react-router-dom';
import { Shell } from './shell/Shell';
import { Launchpad } from './Launchpad';
import { KatalogLayout } from './katalog/KatalogLayout';
import { CatalogList } from './katalog/CatalogList';
import { ItemDetail } from './katalog/ItemDetail';
import { ScanView } from './katalog/ScanView';
import { DownloadsView } from './katalog/DownloadsView';
import { SettingsView } from './katalog/SettingsView';
import { Splash } from './Splash';

// you sign into zaentrum (the portal) once; products + the in-shell katalog
// console ride the same SSO session. Unauthenticated hits bounce to Keycloak.
export function App() {
  const auth = useAuth();

  useEffect(() => {
    if (!auth.isLoading && !auth.isAuthenticated && !auth.activeNavigator && !auth.error) {
      void auth.signinRedirect();
    }
  }, [auth.isLoading, auth.isAuthenticated, auth.activeNavigator, auth.error]);

  if (auth.error) return <Splash message={`sign-in failed: ${auth.error.message}`} />;
  if (!auth.isAuthenticated) return <Splash message="signing you in…" />;

  return (
    <Routes>
      <Route element={<Shell />}>
        <Route index element={<Launchpad />} />
        <Route path="katalog" element={<KatalogLayout />}>
          <Route index element={<CatalogList />} />
          <Route path="item/:id" element={<ItemDetail />} />
          <Route path="scan" element={<ScanView />} />
          <Route path="downloads" element={<DownloadsView />} />
          <Route path="settings" element={<SettingsView />} />
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
