import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Served under /portal on the demo origin (the bare host 302s here). The base
// makes every asset URL + the SPA fallback resolve under /portal/.
export default defineConfig({
  base: '/portal/',
  plugins: [react()],
});
