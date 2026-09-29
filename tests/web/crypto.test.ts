import { describe, it, expect } from "vitest";
import { sanitizeFilename } from "../../src/lib/crypto";

// Mirrors the Go CLI table in cli/main_test.go — both ends MUST agree on the
// same sanitization contract or download names will diverge from uploads.

describe("sanitizeFilename (server) — parity with Go CLI", () => {
  it("strips path traversal", () => {
    expect(sanitizeFilename("../../etc/passwd")).toBe("passwd");
  });

  it("keeps only the basename", () => {
    expect(sanitizeFilename("folder/file.txt")).toBe("file.txt");
  });

  it("falls back for empty and dot-names (parity with Go CLI)", () => {
    expect(sanitizeFilename("")).toBe("file");
    expect(sanitizeFilename(".")).toBe("file");
    expect(sanitizeFilename("..")).toBe("file");
  });

  it("replaces disallowed characters", () => {
    const out = sanitizeFilename("my file (v2)+final.txt");
    expect(out).toMatch(/^[a-zA-Z0-9._-]+$/);
    expect(out.endsWith(".txt")).toBe(true);
  });

  it("null/undefined input falls back to the same token as CLI", () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    expect(sanitizeFilename(undefined as any)).toBe("file");
  });
});
