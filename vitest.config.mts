import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

// Minimal vitest config: the only addition over vitest's defaults is the
// `@/` → `src/` alias, so route-handler tests can import Next.js route
// modules (which use the tsconfig paths alias) alongside lib unit tests.
export default defineConfig({
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    environment: "node",
    include: ["tests/**/*.test.ts"],
  },
});