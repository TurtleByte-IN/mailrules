import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  // The daemon's default MAILRULES_LISTEN; proxying keeps cookies same-origin in dev.
  server: { proxy: { '/api': 'http://127.0.0.1:8080' } },
  test: { environment: 'jsdom' },
});
