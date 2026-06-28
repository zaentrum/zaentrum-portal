import { useEffect } from 'react';
import { useAuth } from 'react-oidc-context';
import { Launchpad } from './Launchpad';
import { Splash } from './Splash';

// you sign into zaentrum (the portal) once; products ride the same SSO session.
// hitting /portal unauthenticated bounces straight to the Keycloak login.
export function App() {
  const auth = useAuth();

  useEffect(() => {
    if (!auth.isLoading && !auth.isAuthenticated && !auth.activeNavigator && !auth.error) {
      void auth.signinRedirect();
    }
  }, [auth.isLoading, auth.isAuthenticated, auth.activeNavigator, auth.error]);

  if (auth.error) return <Splash message={`sign-in failed: ${auth.error.message}`} />;
  if (auth.isAuthenticated) return <Launchpad auth={auth} />;
  return <Splash message="signing you in…" />;
}
