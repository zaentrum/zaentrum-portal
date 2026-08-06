import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { resolve } from 'node:path';

export default defineConfig({
  root: resolve(__dirname),
  plugins: [react()],
  resolve: { alias: { 'react-oidc-context': resolve(__dirname, 'oidc-stub.tsx') } },
  server: { port: 8792, proxy: { '/api/portal': 'http://localhost:8791' } },
});
