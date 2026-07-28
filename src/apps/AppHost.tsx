// Hosts a registered app inside the launchpad shell.
//
// The app is a federated remote: its bundle is fetched through the portal's own
// proxy and its component is rendered directly in THIS React tree. One DOM, one
// React instance (react/react-dom are shared), no iframe, no second origin.
//
// Remotes are attached at runtime rather than declared at build time, because
// which apps exist is a registry decision, not a build-time one.
import { Component, Suspense, useEffect, useMemo, useState, type ReactNode } from 'react';
import { useParams } from 'react-router-dom';
import { useAuth } from 'react-oidc-context';
import { Heading, Spinner, Text } from '@nalet/design-system';
import {
  __federation_method_getRemote,
  __federation_method_setRemote,
} from 'virtual:__federation__';
import { PORTAL_API } from '../lib/api';
import './apphost.css';

/** What an embeddable app's exposed console accepts from its host. */
interface ConsoleProps {
  apiBase: string;
  token: string | undefined;
  onUnauthorized?: () => void;
}
type ConsoleComponent = (props: ConsoleProps) => ReactNode;

/** A federated module can fail long after render starts; keep it contained. */
class RemoteBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    if (this.state.error) return <Failure message={this.state.error.message} />;
    return this.props.children;
  }
}

function Failure({ message }: { message: string }) {
  return (
    <div className="apphost__state">
      <Heading level={2}>this app could not be loaded</Heading>
      <Text variant="muted" as="p">
        {message}
      </Text>
      <Text variant="muted" as="p">
        An app is embeddable only once it has been given a proxy address in the registry.
      </Text>
    </div>
  );
}

export function AppHost() {
  const { key = '' } = useParams();
  const auth = useAuth();
  const token = auth.user?.access_token;
  const [Remote, setRemote] = useState<ConsoleComponent | null>(null);
  const [error, setError] = useState('');

  // Everything the app loads — its bundle and its API — goes through the
  // portal, so the app never needs an origin or a session of its own.
  const base = useMemo(() => `${PORTAL_API}/apps/${encodeURIComponent(key)}/`, [key]);

  useEffect(() => {
    let cancelled = false;
    setRemote(null);
    setError('');

    const entry = new URL(`${base}embed/assets/remoteEntry.js`, window.location.origin).href;
    __federation_method_setRemote(key, {
      url: () => Promise.resolve(entry),
      format: 'esm',
      from: 'vite',
    });

    __federation_method_getRemote(key, './Console')
      .then((mod) => {
        if (cancelled) return;
        const Console = (mod as { Console?: ConsoleComponent; default?: ConsoleComponent })
          .Console;
        if (typeof Console !== 'function') {
          throw new Error('the app does not expose a Console component');
        }
        // Store the component itself — setState would otherwise call it.
        setRemote(() => Console);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      });

    return () => {
      cancelled = true;
    };
  }, [base, key]);

  // The app's stylesheet ships beside its bundle; the shell links it once.
  useEffect(() => {
    const href = `${base}embed/assets/console.css`;
    if (document.querySelector(`link[data-app="${key}"]`)) return;
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = href;
    link.dataset.app = key;
    document.head.appendChild(link);
  }, [base, key]);

  if (error) return <Failure message={error} />;

  return (
    <section className="apphost">
      <RemoteBoundary>
        <Suspense
          fallback={
            <div className="apphost__state">
              <Spinner />
            </div>
          }
        >
          {Remote ? (
            <Remote
              apiBase={base}
              token={token}
              onUnauthorized={() => void auth.signinRedirect()}
            />
          ) : (
            <div className="apphost__state">
              <Spinner />
            </div>
          )}
        </Suspense>
      </RemoteBoundary>
    </section>
  );
}
