// Hosts a registered app inside the launchpad shell.
//
// The app is loaded as a plain ES module through the portal's own proxy and
// mounted into this page — no iframe, and no second origin. The shell stays the
// only thing that talks to the identity provider: it hands the app the proxied
// API base and the signed-in user's token, and keeps that token fresh.
import { useEffect, useRef, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useAuth } from 'react-oidc-context';
import { Heading, Spinner, Text } from '@nalet/design-system';
import { PORTAL_API } from '../lib/api';
import './apphost.css';

/** What an embeddable app must export. */
interface MountHandle {
  update(opts: Record<string, unknown>): void;
  unmount(): void;
}
type MountFn = (el: HTMLElement, opts: Record<string, unknown>) => MountHandle;

export function AppHost() {
  const { key = '' } = useParams();
  const auth = useAuth();
  const token = auth.user?.access_token;
  const hostRef = useRef<HTMLDivElement | null>(null);
  const handleRef = useRef<MountHandle | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);

  // Everything the app loads — its module and its API — goes through the
  // portal, so the app never needs an origin or a session of its own.
  const base = `${PORTAL_API}/apps/${encodeURIComponent(key)}/`;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError('');

    // The module URL is absolute so a relative import inside the bundle (its
    // stylesheet) resolves against the proxy, not this route.
    const moduleUrl = new URL(`${base}embed/console.js`, window.location.origin).href;

    import(/* @vite-ignore */ moduleUrl)
      .then((mod: { mount?: MountFn; default?: MountFn }) => {
        if (cancelled) return;
        const mount = mod.mount ?? mod.default;
        if (typeof mount !== 'function') {
          throw new Error('the app does not export a mount() function');
        }
        if (!hostRef.current) return;
        handleRef.current = mount(hostRef.current, {
          apiBase: base,
          token,
          onUnauthorized: () => void auth.signinRedirect(),
        });
        setLoading(false);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setError(e instanceof Error ? e.message : String(e));
        setLoading(false);
      });

    return () => {
      cancelled = true;
      handleRef.current?.unmount();
      handleRef.current = null;
    };
    // Re-mounting on every token refresh would throw the user's place away;
    // the token is pushed through update() instead.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base]);

  // Silent renew replaces the token — hand the new one over without remounting.
  useEffect(() => {
    handleRef.current?.update({ token });
  }, [token]);

  return (
    <section className="apphost">
      {loading && (
        <div className="apphost__state">
          <Spinner />
        </div>
      )}
      {error && (
        <div className="apphost__state">
          <Heading level={2}>this app could not be loaded</Heading>
          <Text variant="muted" as="p">
            {error}
          </Text>
          <Text variant="muted" as="p">
            An app is embeddable only once it has been given a proxy address in the registry.
          </Text>
        </div>
      )}
      <div ref={hostRef} hidden={!!error} />
    </section>
  );
}
