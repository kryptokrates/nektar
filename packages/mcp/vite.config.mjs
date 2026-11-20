// vite build for the operator dashboard — root is dashboard/, output lands in
// dist/dashboard/ where http.ts serves it from. No server config: the
// production path is always the compiled bundle behind the node:http API.
import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

export default defineConfig({
  root: 'dashboard',
  plugins: [svelte()],
  build: { outDir: '../dist/dashboard', emptyOutDir: true }
})
