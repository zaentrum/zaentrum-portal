// Stands in for react-oidc-context so the console can render without Keycloak.
import type { ReactNode } from 'react';
export function useAuth() {
  return {
    isAuthenticated: true,
    isLoading: false,
    user: { access_token: 'harness', profile: { preferred_username: 'harness' } },
    signinRedirect: () => {},
    signoutRedirect: () => {},
  };
}
export function AuthProvider({ children }: { children: ReactNode }) {
  return <>{children}</>;
}
export function hasAuthParams() { return false; }
