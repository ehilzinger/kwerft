// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies API calls to a local `go run ./cmd/kwerft`.
export default defineConfig({
  plugins: [react()],
  server: {
    // ws: shells are WebSockets.
    proxy: { "/api": { target: "http://localhost:8080", ws: true } },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
