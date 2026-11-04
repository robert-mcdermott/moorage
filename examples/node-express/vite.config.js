import { defineConfig } from "vite";

export default defineConfig({
  // While developing, the page is served by Vite (npm run dev:web) and the
  // API by the server (npm run dev:api).
  server: {
    proxy: { "/api": "http://127.0.0.1:3000" },
  },
});
