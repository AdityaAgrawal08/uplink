import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import {
  parseGoImports,
  parseTsImports,
  buildGraph,
  serializeGraph,
  countLines,
} from "../../scripts/gengraph.mjs";

// Unit + fixture tests for scripts/gengraph.mjs (`npm run graph:gen`), the
// zero-dep generator behind /graph. Runs in the existing vitest path
// (tests/**/*.test.ts), so `make test-web` / CI picks it up with no wiring.

const MOD = "github.com/AdityaAgrawal08/uplink-delta/cli";

function write(root: string, rel: string, contents: string) {
  const abs = join(root, rel);
  mkdirSync(dirname(abs), { recursive: true });
  writeFileSync(abs, contents);
}

describe("parseGoImports", () => {
  it("parses single-line imports (plain, aliased, blank, dot)", () => {
    const src = [
      'package demo',
      '',
      `import "${MOD}/lan"`,
      `import alias "github.com/AdityaAgrawal08/uplink-delta/cli/pkg/crc64"`,
      `import _ "github.com/AdityaAgrawal08/uplink-delta/cli/pkg/tarball"`,
      `import . "github.com/AdityaAgrawal08/uplink-delta/cli/wan"`,
      '',
    ].join("\n");
    expect(parseGoImports(src)).toEqual([
      `${MOD}/lan`,
      `${MOD}/pkg/crc64`,
      `${MOD}/pkg/tarball`,
      `${MOD}/wan`,
    ]);
  });

  it("parses block imports, multiline aliases and raw-string literals", () => {
    const src = [
      "package demo",
      "",
      "import (",
      `\t"fmt"`,
      `\t"${MOD}/lan"`,
      `\talias "${MOD}/pkg/crc64"`,
      `\t_ "${MOD}/pkg/tarball"`,
      `\t. "${MOD}/wan"`,
      "\t`" + `${MOD}/raw` + "`",
      ")",
      "",
      "import (",
      "\talias2",
      `\t"${MOD}/wan"`,
      ")",
      "",
    ].join("\n");
    expect(parseGoImports(src)).toEqual([
      "fmt",
      `${MOD}/lan`,
      `${MOD}/pkg/crc64`,
      `${MOD}/pkg/tarball`,
      `${MOD}/wan`,
      `${MOD}/raw`,
      `${MOD}/wan`,
    ]);
  });

  it("ignores commented-out imports", () => {
    const src = [
      "package demo",
      "",
      `// import "${MOD}/commented"`,
      `/* import "${MOD}/blocked" */`,
      `import "${MOD}/lan" // trailing "${MOD}/trailing"`,
      "",
    ].join("\n");
    expect(parseGoImports(src)).toEqual([`${MOD}/lan`]);
  });
});

describe("parseTsImports", () => {
  it("collects relative, alias, side-effect, dynamic and re-export specifiers", () => {
    const src = [
      'import { a } from "@/lib/alpha";',
      'import b from "./beta";',
      'import "../styles/global.css";',
      'export { c } from "@/components/Widget";',
      'const lazy = import("@/lib/rooms");',
      'import React from "react";',
      "",
    ].join("\n");
    const specs = parseTsImports(src);
    expect(specs).toContain("@/lib/alpha");
    expect(specs).toContain("./beta");
    expect(specs).toContain("../styles/global.css");
    expect(specs).toContain("@/components/Widget");
    expect(specs).toContain("@/lib/rooms");
    expect(specs).toContain("react");
  });
});

describe("countLines", () => {
  it("matches wc -l semantics", () => {
    expect(countLines("")).toBe(0);
    expect(countLines("a")).toBe(1);
    expect(countLines("a\nb")).toBe(2);
    expect(countLines("a\nb\n")).toBe(2);
  });
});

describe("buildGraph (fixture repo)", () => {
  let root: string;

  beforeAll(() => {
    root = mkdtempSync(join(tmpdir(), "gengraph-fixture-"));
    write(root, "cli/main.go", [
      "package main",
      "",
      "import (",
      '\t"fmt"',
      `\t"${MOD}/lan"`,
      `\t_ "${MOD}/pkg/crc64"`,
      ")",
      "",
      "func main() { fmt.Println() }",
      "",
    ].join("\n"));
    write(root, "cli/helper.go", "package main\n");
    write(root, "cli/helper_test.go", `package main\n\nimport "${MOD}/wan"\n`);
    write(root, "cli/lan/server.go", [
      "package lan",
      "",
      `import "${MOD}/wan"`,
      "",
    ].join("\n"));
    write(root, "cli/lan/server_test.go", "package lan\n");
    write(root, "cli/wan/wan.go", "package wan\n");
    write(root, "cli/pkg/crc64/crc64.go", "package crc64\n");
    write(root, "src/lib/alpha.ts", "export const alpha = 1;\n");
    write(root, "src/lib/beta.ts", [
      'import { alpha } from "@/lib/alpha";',
      'import "./alpha";',
      'import lodash from "lodash";',
      "[1, 2].map((n) => import(\"@/components/Widget\"));",
      "",
    ].join("\n"));
    write(root, "src/components/Widget.tsx", [
      'import { alpha } from "../lib/alpha";',
      "export function Widget() { return alpha; }",
      "",
    ].join("\n"));
    write(root, "src/components/Widget.test.tsx", 'import { Widget } from "./Widget";\n');
    write(root, "src/app/page.tsx", 'import { Widget } from "@/components/Widget";\n');
    write(root, "src/env.test.ts", "export {};\n");
    write(root, "src/node_modules/pkg/index.ts", "export {};\n");
    write(root, "src/mocks/data.ts", "export {};\n");
    write(root, "src/lib/fixtures/sample.ts", "export {};\n");
    write(root, "docs/readme.md", "# docs\n");
    write(root, "next.config.ts", "export default {};\n");
  });

  afterAll(() => {
    rmSync(root, { recursive: true, force: true });
  });

  it("includes only non-test cli/*.go and src/*.ts(x) nodes", () => {
    const { nodes } = buildGraph(root);
    const ids = nodes.map((n) => n.id);
    expect(ids).toEqual([
      "cli/helper.go",
      "cli/lan/server.go",
      "cli/main.go",
      "cli/pkg/crc64/crc64.go",
      "cli/wan/wan.go",
      "src/app/page.tsx",
      "src/components/Widget.tsx",
      "src/lib/alpha.ts",
      "src/lib/beta.ts",
    ]);
    expect(ids.some((id) => id.includes("_test.go"))).toBe(false);
    expect(ids.some((id) => id.includes(".test."))).toBe(false);
    expect(ids.some((id) => id.includes("node_modules"))).toBe(false);
    expect(ids.some((id) => id.includes("mocks"))).toBe(false);
    expect(ids.some((id) => id.includes("fixtures"))).toBe(false);
    expect(ids.some((id) => id.startsWith("docs/"))).toBe(false);
    expect(ids.some((id) => id.includes("next.config"))).toBe(false);
  });

  it("groups nodes by top-level dir and sizes them by LOC", () => {
    const { nodes } = buildGraph(root);
    const byId = new Map(nodes.map((n) => [n.id, n]));
    expect(byId.get("cli/main.go")?.group).toBe("cli");
    expect(byId.get("cli/lan/server.go")?.group).toBe("lan");
    expect(byId.get("src/app/page.tsx")?.group).toBe("app");
    expect(byId.get("src/components/Widget.tsx")?.group).toBe("components");
    expect(byId.get("src/lib/alpha.ts")?.group).toBe("lib");
    expect(byId.get("cli/main.go")?.size).toBe(9);
  });

  it("resolves internal Go package imports to package files and drops external ones", () => {
    const { links } = buildGraph(root);
    expect(links).toContainEqual({ source: "cli/main.go", target: "cli/lan/server.go" });
    expect(links).toContainEqual({ source: "cli/main.go", target: "cli/pkg/crc64/crc64.go" });
    expect(links).toContainEqual({ source: "cli/lan/server.go", target: "cli/wan/wan.go" });
    // Test-only files are not valid edge targets.
    expect(links.some((l) => l.target.endsWith("server_test.go"))).toBe(false);
    // External libs ("fmt", "lodash", "react") create no edges.
    expect(links.some((l) => l.target === "fmt" || l.target === "lodash")).toBe(false);
  });

  it("resolves relative and @/ TS imports, including .tsx and dynamic forms", () => {
    const { links } = buildGraph(root);
    expect(links).toContainEqual({ source: "src/lib/beta.ts", target: "src/lib/alpha.ts" });
    expect(links).toContainEqual({ source: "src/components/Widget.tsx", target: "src/lib/alpha.ts" });
    expect(links).toContainEqual({ source: "src/app/page.tsx", target: "src/components/Widget.tsx" });
  });

  it("is deterministic (sorted, deduped, stable bytes)", () => {
    const a = buildGraph(root);
    const b = buildGraph(root);
    expect(serializeGraph(a)).toBe(serializeGraph(b));
    const nodeIds = a.nodes.map((n) => n.id);
    expect([...nodeIds].sort()).toEqual(nodeIds);
    const linkKeys = a.links.map((l) => `${l.source}->${l.target}`);
    expect([...linkKeys].sort()).toEqual(linkKeys);
    expect(new Set(linkKeys).size).toBe(linkKeys.length);
  });
});
