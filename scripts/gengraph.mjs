#!/usr/bin/env node
/**
 * scripts/gengraph.mjs — zero-dependency source dependency graph generator.
 *
 * Scans the repository for first-party source files:
 *   - non-test `.go` files under `cli/` (module github.com/AdityaAgrawal08/uplink-delta/cli)
 *   - non-test `.ts`/`.tsx` files under `src/` (Next.js app; `@/...` aliases `src/`)
 *
 * Parses their imports and writes `public/graph.json` as:
 *   { nodes: [{ id, label, group, size }], links: [{ source, target }] }
 * where `id` is the repo-relative POSIX path, `group` the top-level dir,
 * `size` the line count and links only connect included nodes (external
 * library imports are dropped).
 *
 * Output is deterministic: nodes sorted by id, links sorted + deduped.
 * Used by `npm run graph:gen` and by the Docker build before `npm run build`.
 */

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

/** Go module path of the CLI; imports under it are internal graph edges. */
export const GO_MODULE_PREFIX = "github.com/AdityaAgrawal08/uplink-delta/cli";

/** Only files under these top-level dirs can become nodes. */
const AREA_DIRS = ["cli", "src"];

/** Directory names pruned anywhere in the walk (deps, build output, docs, test data). */
const SKIP_DIR_NAMES = new Set([
  "node_modules",
  ".git",
  ".next",
  ".vercel",
  "out",
  "coverage",
  "dist",
  "build",
  "uploads_dev",
  "docs",
  "deploy",
  "mocks",
  "__mocks__",
  "fixtures",
  "__fixtures__",
  "testdata",
]);

/** Extensions tried when resolving a TS/TSX import specifier to a file node. */
const TS_RESOLVE_EXTS = [".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"];

/** Config-ish file names that must never become nodes even if they land in an area dir. */
const CONFIG_FILE_RE =
  /^(next\.config|tailwind\.config|postcss\.config|tsconfig|eslint\.config|vercel\.json|docker-compose)/;

/** @returns {string} Absolute path to the repo root (parent of scripts/). */
export function repoRoot() {
  return path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
}

/**
 * Count lines the same way `wc -l` does: newline count, with a trailing
 * newline not counting as an extra empty line.
 * @param {string} text
 * @returns {number}
 */
export function countLines(text) {
  if (text.length === 0) return 0;
  let lines = 0;
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) === 10) lines++;
  }
  if (!text.endsWith("\n")) lines++;
  return lines;
}

/**
 * Replace comments with spaces while preserving string literals and line
 * offsets, so import scanning never sees commented-out imports.
 * @param {string} source
 * @returns {string}
 */
export function stripComments(source) {
  const out = [];
  let i = 0;
  /** @type {"code"|"line"|"block"|"single"|"double"|"template"} */
  let state = "code";
  while (i < source.length) {
    const c = source[i];
    const d = i + 1 < source.length ? source[i + 1] : "";
    if (state === "code") {
      if (c === "/" && d === "/") {
        state = "line";
        out.push("  ");
        i += 2;
        continue;
      }
      if (c === "/" && d === "*") {
        state = "block";
        out.push("  ");
        i += 2;
        continue;
      }
      if (c === "'") state = "single";
      else if (c === '"') state = "double";
      else if (c === "`") state = "template";
      out.push(c);
      i++;
      continue;
    }
    if (state === "line") {
      if (c === "\n") {
        state = "code";
        out.push(c);
      } else {
        out.push(" ");
      }
      i++;
      continue;
    }
    if (state === "block") {
      if (c === "*" && d === "/") {
        state = "code";
        out.push("  ");
        i += 2;
        continue;
      }
      out.push(c === "\n" ? "\n" : " ");
      i++;
      continue;
    }
    // Inside a string literal.
    if (c === "\\") {
      out.push(c, d);
      i += 2;
      continue;
    }
    const closes =
      (state === "single" && c === "'") ||
      (state === "double" && c === '"') ||
      (state === "template" && c === "`");
    if (closes) state = "code";
    out.push(c);
    i++;
  }
  return out.join("");
}

/**
 * Extract every import path from Go source, in source order, across all
 * legal forms: single-line, parenthesised blocks, aliased (`alias "p"`),
 * blank (`_ "p"`), dot (`. "p"`) and raw-string (backtick) literals.
 * Commented-out imports are ignored.
 * @param {string} source
 * @returns {string[]}
 */
export function parseGoImports(source) {
  const clean = stripComments(source);
  const specs = [];
  const declRe =
    /\bimport\s*(\([\s\S]*?\)|(?:[A-Za-z_][A-Za-z0-9_]*\s+|[._]\s+)?(?:"[^"\n]*"|`[^`\n]*`))/g;
  for (const decl of clean.matchAll(declRe)) {
    // For a block the body holds every ImportSpec; for a single declaration
    // it holds the optional alias plus the literal. Either way, quoted
    // strings are the import paths (aliases `_` and `.` are not quoted).
    for (const lit of decl[1].matchAll(/"([^"\n]*)"|`([^`\n]*)`/g)) {
      const spec = lit[1] ?? lit[2] ?? "";
      if (spec.length > 0) specs.push(spec);
    }
  }
  return specs;
}

/**
 * Extract every module specifier from TS/TSX source: side-effect imports,
 * `import ... from "..."`, `export ... from "..."`, dynamic `import("...")`
 * and (defensively) `require("...")`. Relative and `@/` specs are returned
 * as written; callers decide what resolves to a node.
 * @param {string} source
 * @returns {string[]}
 */
export function parseTsImports(source) {
  const clean = stripComments(source);
  const specs = new Set();
  const patterns = [
    /\bimport\s*\(\s*["']([^"']+)["']\s*\)/g,
    /\bimport\s+["']([^"']+)["']/g,
    /\bfrom\s*["']([^"']+)["']/g,
    /\brequire\s*\(\s*["']([^"']+)["']\s*\)/g,
  ];
  for (const re of patterns) {
    for (const m of clean.matchAll(re)) specs.add(m[1]);
  }
  return [...specs];
}

/** POSIX dirname for repo-relative ids. @param {string} p @returns {string} */
function posixDirname(p) {
  const i = p.lastIndexOf("/");
  return i === -1 ? "." : p.slice(0, i);
}

/** Top-level dir a node belongs to (`cli`, `app`, `lib`, `components`, `lan`, ...). */
function groupFor(id) {
  const dirParts = id.split("/").slice(0, -1);
  const rest = dirParts.slice(1);
  return rest.length > 0 ? rest[0] : dirParts[0];
}

/**
 * Walk `cli/` and `src/` under `rootDir`, returning sorted repo-relative
 * POSIX ids of every file that qualifies as a graph node.
 * @param {string} rootDir
 * @returns {string[]}
 */
function collectSourceFiles(rootDir) {
  /** @type {string[]} */
  const ids = [];
  /**
   * @param {string} absDir
   * @param {string} relDir
   */
  const visit = (absDir, relDir) => {
    let entries;
    try {
      entries = fs.readdirSync(absDir, { withFileTypes: true });
    } catch {
      return;
    }
    const area = relDir.split("/")[0];
    for (const entry of entries) {
      const name = entry.name;
      if (entry.isDirectory()) {
        if (name.startsWith(".") || SKIP_DIR_NAMES.has(name)) continue;
        visit(path.join(absDir, name), `${relDir}/${name}`);
        continue;
      }
      if (!entry.isFile()) continue;
      if (area === "cli") {
        if (name.endsWith(".go") && !name.endsWith("_test.go")) {
          ids.push(`${relDir}/${name}`);
        }
        continue;
      }
      if (area === "src") {
        const isTs = name.endsWith(".ts") || name.endsWith(".tsx");
        const isTest = /\.(test|spec)\.tsx?$/.test(name);
        if (
          isTs &&
          !isTest &&
          !name.endsWith(".d.ts") &&
          !name.startsWith(".env") &&
          !CONFIG_FILE_RE.test(name)
        ) {
          ids.push(`${relDir}/${name}`);
        }
      }
    }
  };
  for (const area of AREA_DIRS) {
    const abs = path.join(rootDir, area);
    if (fs.existsSync(abs) && fs.statSync(abs).isDirectory()) visit(abs, area);
  }
  ids.sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
  return ids;
}

/**
 * Resolve a Go import path to included node ids. Internal package imports
 * map to every non-test file in the target directory; external imports
 * return [].
 * @param {string} spec
 * @param {Map<string, string[]>} goFilesByDir
 * @returns {string[]}
 */
function goImportTargets(spec, goFilesByDir) {
  let rel;
  if (spec === GO_MODULE_PREFIX) rel = "cli";
  else if (spec.startsWith(`${GO_MODULE_PREFIX}/`)) rel = `cli/${spec.slice(GO_MODULE_PREFIX.length + 1)}`;
  else return [];
  return goFilesByDir.get(rel) ?? [];
}

/**
 * Resolve a TS/TSX specifier to included node ids: relative (`./x`, `../x`)
 * and `@/` alias paths only; bare package specifiers return [].
 * @param {string} importerId
 * @param {string} spec
 * @param {Set<string>} idSet
 * @returns {string[]}
 */
function tsImportTargets(importerId, spec, idSet) {
  let base;
  if (spec.startsWith("@/")) {
    base = `src/${spec.slice(2)}`;
  } else if (spec.startsWith("./") || spec.startsWith("../")) {
    base = path.posix.normalize(path.posix.join(posixDirname(importerId), spec));
  } else {
    return [];
  }
  const candidates = new Set([base]);
  for (const ext of TS_RESOLVE_EXTS) {
    candidates.add(`${base}${ext}`);
    candidates.add(`${base}/index${ext}`);
  }
  const swapped = base.replace(/\.(js|jsx|mjs|cjs)$/, "");
  if (swapped !== base) {
    for (const ext of TS_RESOLVE_EXTS) candidates.add(`${swapped}${ext}`);
  }
  return [...candidates].filter((c) => idSet.has(c));
}

/**
 * Build the full graph for a repo checkout.
 * @param {string} [rootDir]
 * @returns {{nodes: Array<{id: string, label: string, group: string, size: number}>, links: Array<{source: string, target: string}>}}
 */
export function buildGraph(rootDir = repoRoot()) {
  const ids = collectSourceFiles(rootDir);
  const idSet = new Set(ids);

  /** @type {Map<string, string[]>} */
  const goFilesByDir = new Map();
  for (const id of ids) {
    if (!id.endsWith(".go")) continue;
    const dir = posixDirname(id);
    const list = goFilesByDir.get(dir);
    if (list) list.push(id);
    else goFilesByDir.set(dir, [id]);
  }

  /** @type {Array<{id: string, label: string, group: string, size: number}>} */
  const nodes = [];
  /** @type {Array<{source: string, target: string}>} */
  const links = [];
  const linkKeys = new Set();
  const addLink = (source, target) => {
    if (source === target || !idSet.has(source) || !idSet.has(target)) return;
    const key = `${source}\u0000${target}`;
    if (linkKeys.has(key)) return;
    linkKeys.add(key);
    links.push({ source, target });
  };

  for (const id of ids) {
    const source = fs.readFileSync(path.join(rootDir, id), "utf8");
    nodes.push({
      id,
      label: id.split("/").pop() ?? id,
      group: groupFor(id),
      size: countLines(source),
    });
    if (id.endsWith(".go")) {
      for (const spec of parseGoImports(source)) {
        for (const target of goImportTargets(spec, goFilesByDir)) addLink(id, target);
      }
    } else {
      for (const spec of parseTsImports(source)) {
        for (const target of tsImportTargets(id, spec, idSet)) addLink(id, target);
      }
    }
  }

  nodes.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  links.sort((a, b) => {
    if (a.source !== b.source) return a.source < b.source ? -1 : 1;
    if (a.target !== b.target) return a.target < b.target ? -1 : 1;
    return 0;
  });
  return { nodes, links };
}

/**
 * Deterministic JSON serialization for the graph payload.
 * @param {{nodes: unknown[], links: unknown[]}} graph
 * @returns {string}
 */
export function serializeGraph(graph) {
  return `${JSON.stringify({ nodes: graph.nodes, links: graph.links }, null, 2)}\n`;
}

/**
 * Build the graph and write it to `public/graph.json` (or `outFile`).
 * @param {string} [rootDir]
 * @param {string} [outFile]
 * @returns {{graph: ReturnType<typeof buildGraph>, outFile: string}}
 */
export function generate(rootDir = repoRoot(), outFile = path.join(rootDir, "public", "graph.json")) {
  const graph = buildGraph(rootDir);
  fs.mkdirSync(path.dirname(outFile), { recursive: true });
  fs.writeFileSync(outFile, serializeGraph(graph));
  return { graph, outFile };
}

const invokedDirectly =
  typeof process !== "undefined" &&
  Boolean(process.argv[1]) &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;

if (invokedDirectly) {
  const { graph, outFile } = generate();
  const rel = path.relative(process.cwd(), outFile) || outFile;
  console.log(`graph:gen — wrote ${rel} (${graph.nodes.length} nodes, ${graph.links.length} links)`);
}
