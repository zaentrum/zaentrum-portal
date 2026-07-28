import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
// The plugin ships a CJS default export; the namespace import keeps
// TypeScript's ESM view of it callable.
import federationNs from '@originjs/vite-plugin-federation';

const federation = federationNs as unknown as typeof import('@originjs/vite-plugin-federation').default;

// Served under /portal on the demo origin (the bare host 302s here). The base
// makes every asset URL + the SPA fallback resolve under /portal/.
export default defineConfig({
  base: '/portal/',
  plugins: [
    react(),
    // Federation HOST. Remotes are not known at build time — the launchpad
    // registry decides which apps exist, and their bundles are fetched through
    // the portal's own proxy — so they are attached at runtime (see AppHost).
    // react/react-dom are shared so the shell and every app it hosts render
    // from ONE React instance; hooks and context depend on that.
    federation({
      name: 'portal-shell',
      // A placeholder remote is declared so the plugin emits its shared-scope
      // bootstrap; with an empty map it leaves an unresolved placeholder and
      // every runtime remote fails with "shareScope is not defined". The real
      // remotes are attached at runtime by AppHost and this one is never
      // fetched.
      remotes: {
        __placeholder__: {
          external: 'Promise.resolve("")',
          externalType: 'promise',
          format: 'esm',
          from: 'vite',
        },
      },
      shared: {
        react: { requiredVersion: '^18.3.0' },
        'react-dom': { requiredVersion: '^18.3.0' },
      },
    }),
  ],
  build: {
    // Federation emits ES modules that use top-level await.
    target: 'esnext',
    modulePreload: false,
  },
});
