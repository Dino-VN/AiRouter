import path from "path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// The build lands straight in the Go package that embeds it, so shipping is
// "vite build" followed by "go build" and nothing else.
const outDir = path.resolve(__dirname, "../internal/webui/dist")

// Where `bun run dev` forwards API traffic. Point it elsewhere with
// AIHUB_DEV_SERVER when the Go process listens on another address.
const api = process.env.AIHUB_DEV_SERVER ?? "http://127.0.0.1:8317"

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  build: {
    outDir,
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": api,
      "/oauth": api,
      "/healthz": api,
      "/v1": api,
      "/v1beta": api,
    },
  },
})
