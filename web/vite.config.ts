import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

export default defineConfig(({ mode }) => {
  // Same variable and default as the daemon, so both follow one setting.
  const daemon = loadEnv(mode, '.', 'MAILRULES_').MAILRULES_LISTEN ?? '127.0.0.1:8080';
  return {
    plugins: [svelte(), tailwindcss()],
    // Proxying to the daemon keeps cookies same-origin in dev.
    server: { proxy: { '/api': 'http://' + daemon } },
    test: { environment: 'jsdom' },
  };
});
