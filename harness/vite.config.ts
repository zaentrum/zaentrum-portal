import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'node:path';

// HARNESS_PORT and MOCK_PORT move the harness and the mock it proxies to off
// their defaults, when those are taken.
const port = Number(process.env.HARNESS_PORT) || 8792;
const mock = Number(process.env.MOCK_PORT) || 8791;

export default defineConfig({
  root: resolve(__dirname),
  plugins: [react()],
  resolve: { alias: { 'react-oidc-context': resolve(__dirname, 'oidc-stub.tsx') } },
  server: { port, proxy: { '/api/portal': `http://localhost:${mock}` } },
});
