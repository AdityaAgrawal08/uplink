"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import type {
  CSSProperties,
  KeyboardEvent as ReactKeyboardEvent,
  PointerEvent as ReactPointerEvent,
} from "react";

// Interactive, Obsidian-like dependency graph for /graph.
// Data: public/graph.json (see scripts/gengraph.mjs, `npm run graph:gen`).
// Rendering: hand-rolled canvas force layout — no charting/force deps.

interface GraphNode {
  id: string;
  label: string;
  group: string;
  size: number;
}

interface GraphLink {
  source: string;
  target: string;
}

interface GraphData {
  nodes: GraphNode[];
  links: GraphLink[];
}

interface SimNode extends GraphNode {
  x: number;
  y: number;
  vx: number;
  vy: number;
  r: number;
  pinned: boolean;
}

interface SimLink {
  source: SimNode;
  target: SimNode;
}

interface ViewState {
  scale: number;
  tx: number;
  ty: number;
}

type DragState =
  | { kind: "none" }
  | {
      kind: "pan";
      startX: number;
      startY: number;
      originTx: number;
      originTy: number;
      moved: boolean;
    }
  | { kind: "node"; node: SimNode; startX: number; startY: number; moved: boolean };

interface UiState {
  selectedId: string | null;
  hoverId: string | null;
  query: string;
  hiddenGroups: Set<string>;
}

const BG = "#0a0d13";
const LINK_BASE = "#3b4256";
const LINK_ACTIVE = "#9aa4bf";
const LABEL = "#c9d1d9";
const DIM_ALPHA = 0.13;
const PALETTE = [
  "#7f6df2",
  "#4ea1ff",
  "#3fb950",
  "#e3b341",
  "#f778ba",
  "#ff7b72",
  "#39c5cf",
  "#a371f7",
  "#56d364",
  "#db6d28",
];

function groupColor(group: string): string {
  let hash = 0;
  for (let i = 0; i < group.length; i++) hash = (hash * 31 + group.charCodeAt(i)) | 0;
  return PALETTE[Math.abs(hash) % PALETTE.length];
}

function nodeRadius(size: number): number {
  return Math.min(26, 3.5 + Math.sqrt(Math.max(0, size)) * 0.32);
}

function clamp(value: number, lo: number, hi: number): number {
  return value < lo ? lo : value > hi ? hi : value;
}

const styles: Record<string, CSSProperties> = {
  shell: {
    position: "fixed",
    inset: 0,
    background: BG,
    overflow: "hidden",
    fontFamily:
      "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
    color: "#e6edf3",
  },
  canvas: {
    position: "absolute",
    inset: 0,
    display: "block",
    touchAction: "none",
  },
  topBar: {
    position: "absolute",
    top: 12,
    left: 12,
    right: 12,
    display: "flex",
    alignItems: "center",
    gap: 10,
    padding: "8px 12px",
    background: "rgba(17, 22, 34, 0.88)",
    border: "1px solid rgba(255, 255, 255, 0.08)",
    borderRadius: 10,
    backdropFilter: "blur(8px)",
    zIndex: 3,
    pointerEvents: "auto",
  },
  backLink: {
    color: "#a371f7",
    textDecoration: "none",
    fontSize: 16,
    padding: "0 6px",
  },
  title: { fontWeight: 700, fontSize: 14, whiteSpace: "nowrap" },
  search: {
    flex: "1 1 220px",
    minWidth: 120,
    background: "#0d1220",
    border: "1px solid #26304a",
    borderRadius: 7,
    color: "#e6edf3",
    fontSize: 12,
    padding: "6px 10px",
    outline: "none",
  },
  meta: { fontSize: 11, color: "#8b949e", whiteSpace: "nowrap" },
  legend: {
    position: "absolute",
    left: 12,
    bottom: 12,
    display: "flex",
    flexWrap: "wrap",
    gap: 6,
    maxWidth: 420,
    padding: 8,
    background: "rgba(17, 22, 34, 0.85)",
    border: "1px solid rgba(255, 255, 255, 0.08)",
    borderRadius: 10,
    backdropFilter: "blur(8px)",
    zIndex: 3,
  },
  legendItem: {
    display: "inline-flex",
    alignItems: "center",
    gap: 6,
    background: "transparent",
    border: "1px solid #26304a",
    borderRadius: 6,
    color: "#c9d1d9",
    cursor: "pointer",
    fontSize: 11,
    padding: "3px 7px",
  },
  legendCount: { color: "#8b949e" },
  dot: { width: 8, height: 8, borderRadius: "50%", display: "inline-block" },
  panel: {
    position: "absolute",
    top: 64,
    right: 12,
    bottom: 12,
    width: 340,
    maxWidth: "calc(100vw - 24px)",
    overflowY: "auto",
    padding: 14,
    background: "rgba(15, 20, 32, 0.94)",
    border: "1px solid rgba(255, 255, 255, 0.1)",
    borderRadius: 12,
    backdropFilter: "blur(10px)",
    zIndex: 4,
    boxShadow: "0 16px 40px rgba(0, 0, 0, 0.45)",
  },
  panelHeader: {
    display: "flex",
    alignItems: "flex-start",
    justifyContent: "space-between",
    gap: 8,
  },
  panelTitle: {
    fontSize: 14,
    fontWeight: 700,
    wordBreak: "break-all",
  },
  panelPath: {
    fontSize: 11,
    color: "#8b949e",
    marginTop: 4,
    wordBreak: "break-all",
  },
  panelClose: {
    background: "transparent",
    border: "none",
    color: "#8b949e",
    cursor: "pointer",
    fontSize: 18,
    lineHeight: 1,
    padding: 2,
  },
  panelMetaRow: {
    display: "flex",
    alignItems: "center",
    gap: 8,
    margin: "10px 0 4px",
  },
  badge: {
    border: "1px solid",
    borderRadius: 5,
    fontSize: 10,
    fontWeight: 700,
    padding: "1px 6px",
    textTransform: "uppercase",
    letterSpacing: "0.04em",
  },
  panelMeta: { fontSize: 11, color: "#8b949e" },
  section: { marginTop: 14 },
  sectionTitle: {
    fontSize: 11,
    fontWeight: 700,
    color: "#8b949e",
    textTransform: "uppercase",
    letterSpacing: "0.05em",
    marginBottom: 6,
  },
  panelItem: {
    display: "block",
    width: "100%",
    textAlign: "left",
    background: "rgba(255, 255, 255, 0.03)",
    border: "1px solid rgba(255, 255, 255, 0.06)",
    borderRadius: 6,
    color: "#c9d1d9",
    cursor: "pointer",
    fontFamily: "inherit",
    fontSize: 11,
    lineHeight: 1.5,
    marginBottom: 4,
    padding: "4px 7px",
    wordBreak: "break-all",
  },
  empty: { fontSize: 11, color: "#57606a", fontStyle: "italic" },
  hint: {
    marginTop: 16,
    paddingTop: 10,
    borderTop: "1px solid rgba(255, 255, 255, 0.06)",
    fontSize: 10,
    color: "#57606a",
    lineHeight: 1.6,
  },
  error: {
    position: "absolute",
    top: "50%",
    left: "50%",
    transform: "translate(-50%, -50%)",
    maxWidth: 460,
    textAlign: "center",
    fontSize: 12,
    lineHeight: 1.7,
    color: "#f0b6b6",
    background: "rgba(40, 18, 22, 0.9)",
    border: "1px solid rgba(248, 81, 73, 0.4)",
    borderRadius: 10,
    padding: "14px 18px",
    zIndex: 5,
  },
  loading: {
    position: "absolute",
    top: "50%",
    left: "50%",
    transform: "translate(-50%, -50%)",
    fontSize: 12,
    color: "#8b949e",
    zIndex: 2,
  },
};

export default function CodeGraph() {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const ctxRef = useRef<CanvasRenderingContext2D | null>(null);

  const [data, setData] = useState<GraphData | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [hoverId, setHoverId] = useState<string | null>(null);
  const [hiddenGroups, setHiddenGroups] = useState<string[]>([]);

  const nodesRef = useRef<SimNode[]>([]);
  const linksRef = useRef<SimLink[]>([]);
  const nodeByIdRef = useRef<Map<string, SimNode>>(new Map());
  const adjacencyRef = useRef<Map<string, Set<string>>>(new Map());
  const viewRef = useRef<ViewState>({ scale: 1, tx: 0, ty: 0 });
  const sizeRef = useRef({ w: 1, h: 1, dpr: 1 });
  const alphaRef = useRef(0);
  const dirtyRef = useRef(true);
  const rafRef = useRef(0);
  const lastNowRef = useRef(0);
  const visibleRef = useRef(true);
  const dragRef = useRef<DragState>({ kind: "none" });
  const uiRef = useRef<UiState>({
    selectedId: null,
    hoverId: null,
    query: "",
    hiddenGroups: new Set(),
  });
  const frameRef = useRef<(now: number) => void>(() => {});

  // ── Simulation ────────────────────────────────────────────────────────────

  const simulate = useCallback((dt: number) => {
    const nodes = nodesRef.current;
    const links = linksRef.current;
    const hidden = uiRef.current.hiddenGroups;
    const alpha = alphaRef.current;
    const repulsion = 2400;
    const springK = 0.02;
    const restLength = 92;
    const gravity = 0.0016;

    for (const n of nodes) {
      if (n.pinned || hidden.has(n.group)) continue;
      n.vx -= n.x * gravity * alpha;
      n.vy -= n.y * gravity * alpha;
    }

    for (let i = 0; i < nodes.length; i++) {
      const a = nodes[i];
      if (hidden.has(a.group)) continue;
      for (let j = i + 1; j < nodes.length; j++) {
        const b = nodes[j];
        if (hidden.has(b.group)) continue;
        let dx = a.x - b.x;
        let dy = a.y - b.y;
        let d2 = dx * dx + dy * dy;
        if (d2 < 1) {
          dx = 0.5 + i * 0.01;
          dy = 0.5 + j * 0.01;
          d2 = dx * dx + dy * dy;
        }
        const d = Math.sqrt(d2);
        const f = Math.min(repulsion / d2, 12) * alpha;
        const fx = (dx / d) * f;
        const fy = (dy / d) * f;
        if (!a.pinned) {
          a.vx += fx;
          a.vy += fy;
        }
        if (!b.pinned) {
          b.vx -= fx;
          b.vy -= fy;
        }
      }
    }

    for (const link of links) {
      const a = link.source;
      const b = link.target;
      if (hidden.has(a.group) || hidden.has(b.group)) continue;
      const dx = b.x - a.x;
      const dy = b.y - a.y;
      const d = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (d - restLength) * springK * alpha;
      const fx = (dx / d) * f;
      const fy = (dy / d) * f;
      if (!a.pinned) {
        a.vx += fx;
        a.vy += fy;
      }
      if (!b.pinned) {
        b.vx -= fx;
        b.vy -= fy;
      }
    }

    const damp = Math.pow(0.86, dt);
    for (const n of nodes) {
      if (n.pinned || hidden.has(n.group)) {
        n.vx = 0;
        n.vy = 0;
        continue;
      }
      n.vx *= damp;
      n.vy *= damp;
      const speed = Math.hypot(n.vx, n.vy);
      if (speed > 24) {
        n.vx = (n.vx / speed) * 24;
        n.vy = (n.vy / speed) * 24;
      }
      n.x += n.vx * dt;
      n.y += n.vy * dt;
    }
  }, []);

  // ── Rendering ─────────────────────────────────────────────────────────────

  const draw = useCallback(() => {
    const canvas = canvasRef.current;
    const ctx = ctxRef.current;
    if (!canvas || !ctx) return;
    const { w, h, dpr } = sizeRef.current;
    const view = viewRef.current;
    const ui = uiRef.current;
    const hidden = ui.hiddenGroups;

    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = BG;
    ctx.fillRect(0, 0, w, h);

    ctx.save();
    ctx.translate(view.tx, view.ty);
    ctx.scale(view.scale, view.scale);

    const focusId = ui.hoverId ?? ui.selectedId;
    const focus = focusId ? nodeByIdRef.current.get(focusId) : undefined;
    const neighbors = focus ? adjacencyRef.current.get(focus.id) : undefined;
    const q = ui.query.trim().toLowerCase();
    const matches =
      q.length > 0
        ? new Set(
            nodesRef.current
              .filter(
                (n) =>
                  n.id.toLowerCase().includes(q) || n.label.toLowerCase().includes(q),
              )
              .map((n) => n.id),
          )
        : null;

    const emphasisOf = (n: SimNode): number => {
      if (hidden.has(n.group)) return 0;
      if (matches) return matches.has(n.id) ? 1 : DIM_ALPHA;
      if (focus) {
        if (n.id === focus.id) return 1;
        if (neighbors?.has(n.id)) return 1;
        return DIM_ALPHA;
      }
      return 1;
    };

    // Edges.
    ctx.lineWidth = 1 / view.scale;
    for (const link of linksRef.current) {
      const e = Math.min(emphasisOf(link.source), emphasisOf(link.target));
      if (e <= 0) continue;
      const incident = focus
        ? link.source.id === focus.id || link.target.id === focus.id
        : false;
      let alpha = 0.22;
      if (focus && !matches) alpha = incident ? 0.85 : 0.05;
      if (matches && !focus) {
        alpha =
          matches.has(link.source.id) && matches.has(link.target.id) ? 0.5 : 0.04;
      }
      if (matches && focus) {
        alpha =
          incident && matches.has(link.source.id) && matches.has(link.target.id)
            ? 0.85
            : 0.04;
      }
      ctx.globalAlpha = alpha;
      ctx.strokeStyle = incident ? LINK_ACTIVE : LINK_BASE;
      ctx.beginPath();
      ctx.moveTo(link.source.x, link.source.y);
      ctx.lineTo(link.target.x, link.target.y);
      ctx.stroke();
    }

    // Nodes.
    for (const n of nodesRef.current) {
      const e = emphasisOf(n);
      if (e <= 0) continue;
      const selected = n.id === ui.selectedId;
      const hovered = n.id === ui.hoverId;
      ctx.globalAlpha = e;
      ctx.beginPath();
      ctx.arc(n.x, n.y, n.r, 0, Math.PI * 2);
      ctx.fillStyle = groupColor(n.group);
      ctx.fill();
      ctx.lineWidth = (selected ? 2.5 : hovered ? 2 : 1) / view.scale;
      ctx.strokeStyle = selected
        ? "#ffffff"
        : hovered
          ? "#e6edf3"
          : "rgba(230, 237, 243, 0.25)";
      ctx.stroke();
      if (n.pinned && !selected) {
        ctx.globalAlpha = e * 0.9;
        ctx.beginPath();
        ctx.arc(n.x, n.y, n.r + 4 / view.scale, 0, Math.PI * 2);
        ctx.lineWidth = 1 / view.scale;
        ctx.strokeStyle = "rgba(255, 255, 255, 0.45)";
        ctx.stroke();
      }
    }

    // Labels: prominent nodes only, thresholds scale with zoom.
    const fontSize = 11 / view.scale;
    ctx.font = `500 ${fontSize}px ui-monospace, SFMono-Regular, Menlo, monospace`;
    ctx.textAlign = "center";
    ctx.textBaseline = "top";
    for (const n of nodesRef.current) {
      const e = emphasisOf(n);
      if (e <= 0.5) continue;
      const selected = n.id === ui.selectedId;
      const hovered = n.id === ui.hoverId;
      const prominent =
        n.size >= 400 || (view.scale >= 1.2 && n.size >= 150) || view.scale >= 1.8;
      if (!selected && !hovered && !prominent && !matches?.has(n.id)) continue;
      const text = n.label.length > 28 ? `${n.label.slice(0, 26)}…` : n.label;
      ctx.globalAlpha = selected || hovered ? 1 : 0.75 * e;
      ctx.fillStyle = selected ? "#ffffff" : LABEL;
      ctx.fillText(text, n.x, n.y + n.r + 3 / view.scale);
    }

    ctx.globalAlpha = 1;
    ctx.restore();
  }, []);

  const frame = useCallback(
    (now: number) => {
      rafRef.current = 0;
      if (!visibleRef.current) return;
      const dt = clamp((now - lastNowRef.current) / 16.6667, 0.25, 2.5);
      lastNowRef.current = now;
      let active = false;
      if (alphaRef.current > 0.002) {
        simulate(dt);
        alphaRef.current *= Math.pow(0.988, dt);
        active = true;
      }
      if (active || dirtyRef.current) {
        draw();
        dirtyRef.current = false;
      }
      if (active || dirtyRef.current) {
        rafRef.current = requestAnimationFrame(frameRef.current);
      }
    },
    [simulate, draw],
  );

  useEffect(() => {
    frameRef.current = frame;
  }, [frame]);

  const ensureLoop = useCallback(() => {
    if (typeof window === "undefined") return;
    if (rafRef.current !== 0 || !visibleRef.current) return;
    lastNowRef.current = performance.now();
    rafRef.current = requestAnimationFrame(frameRef.current);
  }, []);

  const requestDraw = useCallback(
    (wake = false) => {
      if (wake) alphaRef.current = Math.max(alphaRef.current, 0.5);
      dirtyRef.current = true;
      ensureLoop();
    },
    [ensureLoop],
  );

  // ── Interactions ──────────────────────────────────────────────────────────

  const pickNode = useCallback((sx: number, sy: number): SimNode | null => {
    const view = viewRef.current;
    const wx = (sx - view.tx) / view.scale;
    const wy = (sy - view.ty) / view.scale;
    const hidden = uiRef.current.hiddenGroups;
    let best: SimNode | null = null;
    let bestDist = Infinity;
    for (const n of nodesRef.current) {
      if (hidden.has(n.group)) continue;
      const d = Math.hypot(n.x - wx, n.y - wy);
      if (d <= Math.max(n.r + 6, 12) && d < bestDist) {
        best = n;
        bestDist = d;
      }
    }
    return best;
  }, []);

  const centerOn = useCallback(
    (node: SimNode) => {
      const { w, h } = sizeRef.current;
      const view = viewRef.current;
      view.scale = Math.max(view.scale, 1);
      view.tx = w / 2 - node.x * view.scale;
      view.ty = h / 2 - node.y * view.scale;
      dirtyRef.current = true;
      ensureLoop();
    },
    [ensureLoop],
  );

  const selectNode = useCallback(
    (node: SimNode) => {
      const prevId = uiRef.current.selectedId;
      if (prevId && prevId !== node.id) {
        const prev = nodeByIdRef.current.get(prevId);
        if (prev) {
          prev.pinned = false;
          prev.vx = 0;
          prev.vy = 0;
        }
      }
      node.pinned = true;
      node.vx = 0;
      node.vy = 0;
      uiRef.current = { ...uiRef.current, selectedId: node.id };
      setSelectedId(node.id);
      requestDraw(true);
    },
    [requestDraw],
  );

  const clearSelection = useCallback(() => {
    const id = uiRef.current.selectedId;
    if (id) {
      const node = nodeByIdRef.current.get(id);
      if (node) {
        node.pinned = false;
        node.vx = 0;
        node.vy = 0;
      }
    }
    uiRef.current = { ...uiRef.current, selectedId: null };
    setSelectedId(null);
    requestDraw(true);
  }, [requestDraw]);

  const jumpTo = useCallback(
    (id: string) => {
      const node = nodeByIdRef.current.get(id);
      if (!node) return;
      setHiddenGroups((prev) => prev.filter((g) => g !== node.group));
      selectNode(node);
      centerOn(node);
      requestDraw(true);
    },
    [selectNode, centerOn, requestDraw],
  );

  const onPointerDown = useCallback(
    (event: ReactPointerEvent<HTMLCanvasElement>) => {
      if (event.button !== 0) return;
      const canvas = canvasRef.current;
      if (!canvas) return;
      const rect = canvas.getBoundingClientRect();
      const sx = event.clientX - rect.left;
      const sy = event.clientY - rect.top;
      try {
        canvas.setPointerCapture(event.pointerId);
      } catch {
        // pointer capture is best-effort
      }
      const node = pickNode(sx, sy);
      if (node) {
        dragRef.current = { kind: "node", node, startX: sx, startY: sy, moved: false };
      } else {
        const view = viewRef.current;
        dragRef.current = {
          kind: "pan",
          startX: sx,
          startY: sy,
          originTx: view.tx,
          originTy: view.ty,
          moved: false,
        };
      }
    },
    [pickNode],
  );

  const onPointerMove = useCallback(
    (event: ReactPointerEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      if (!canvas) return;
      const rect = canvas.getBoundingClientRect();
      const sx = event.clientX - rect.left;
      const sy = event.clientY - rect.top;
      const drag = dragRef.current;
      if (drag.kind === "node") {
        const dx = sx - drag.startX;
        const dy = sy - drag.startY;
        if (!drag.moved && dx * dx + dy * dy > 16) drag.moved = true;
        if (drag.moved) {
          const view = viewRef.current;
          drag.node.x = (sx - view.tx) / view.scale;
          drag.node.y = (sy - view.ty) / view.scale;
          drag.node.vx = 0;
          drag.node.vy = 0;
          drag.node.pinned = true;
          requestDraw(true);
        }
        return;
      }
      if (drag.kind === "pan") {
        const dx = sx - drag.startX;
        const dy = sy - drag.startY;
        if (!drag.moved && dx * dx + dy * dy > 4) drag.moved = true;
        if (drag.moved) {
          const view = viewRef.current;
          view.tx = drag.originTx + dx;
          view.ty = drag.originTy + dy;
          requestDraw(false);
        }
        return;
      }
      const node = pickNode(sx, sy);
      const nextHover = node ? node.id : null;
      if (uiRef.current.hoverId !== nextHover) {
        uiRef.current = { ...uiRef.current, hoverId: nextHover };
        setHoverId(nextHover);
        requestDraw(false);
      }
    },
    [pickNode, requestDraw],
  );

  const onPointerUp = useCallback(
    (event: ReactPointerEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      if (canvas) {
        try {
          canvas.releasePointerCapture(event.pointerId);
        } catch {
          // capture may already be released
        }
      }
      const drag = dragRef.current;
      dragRef.current = { kind: "none" };
      if (drag.kind === "node") {
        selectNode(drag.node);
      } else if (drag.kind === "pan" && !drag.moved) {
        clearSelection();
      }
    },
    [selectNode, clearSelection],
  );

  const onPointerLeave = useCallback(() => {
    if (dragRef.current.kind !== "none") return;
    if (uiRef.current.hoverId !== null) {
      uiRef.current = { ...uiRef.current, hoverId: null };
      setHoverId(null);
      requestDraw(false);
    }
  }, [requestDraw]);

  const toggleGroup = useCallback(
    (group: string) => {
      setHiddenGroups((prev) =>
        prev.includes(group) ? prev.filter((g) => g !== group) : [...prev, group],
      );
    },
    [],
  );

  const onSearchKeyDown = useCallback(
    (event: ReactKeyboardEvent<HTMLInputElement>) => {
      if (event.key !== "Enter" || !data) return;
      const q = query.trim().toLowerCase();
      if (!q) return;
      const matches = data.nodes.filter(
        (n) => n.id.toLowerCase().includes(q) || n.label.toLowerCase().includes(q),
      );
      if (matches.length === 0) return;
      const target =
        matches.find((n) => n.label.toLowerCase().startsWith(q)) ??
        matches.find((n) => n.id.toLowerCase().startsWith(q)) ??
        matches[0];
      const node = nodeByIdRef.current.get(target.id);
      if (!node) return;
      setHiddenGroups((prev) => prev.filter((g) => g !== target.group));
      selectNode(node);
      centerOn(node);
      requestDraw(true);
    },
    [data, query, selectNode, centerOn, requestDraw],
  );

  // ── Data loading + layout bootstrap ───────────────────────────────────────

  useEffect(() => {
    let cancelled = false;
    fetch("/graph.json")
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return res.json() as Promise<GraphData>;
      })
      .then((payload) => {
        if (cancelled) return;
        if (!payload || !Array.isArray(payload.nodes) || !Array.isArray(payload.links)) {
          throw new Error("malformed graph.json");
        }
        setData(payload);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : "failed to load graph.json");
        }
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!data) return;
    const map = new Map<string, SimNode>();
    data.nodes.forEach((n, i) => {
      const angle = i * 2.399963229728653;
      const radius = 14 * Math.sqrt(i);
      map.set(n.id, {
        ...n,
        r: nodeRadius(n.size),
        x: Math.cos(angle) * radius,
        y: Math.sin(angle) * radius,
        vx: 0,
        vy: 0,
        pinned: false,
      });
    });
    const simLinks: SimLink[] = [];
    const adjacency = new Map<string, Set<string>>();
    for (const link of data.links) {
      const source = map.get(link.source);
      const target = map.get(link.target);
      if (!source || !target || source === target) continue;
      simLinks.push({ source, target });
      if (!adjacency.has(source.id)) adjacency.set(source.id, new Set());
      if (!adjacency.has(target.id)) adjacency.set(target.id, new Set());
      adjacency.get(source.id)?.add(target.id);
      adjacency.get(target.id)?.add(source.id);
    }
    nodesRef.current = [...map.values()];
    linksRef.current = simLinks;
    nodeByIdRef.current = map;
    adjacencyRef.current = adjacency;
    uiRef.current = {
      selectedId: null,
      hoverId: null,
      query: "",
      hiddenGroups: new Set(),
    };
    alphaRef.current = 1;
    requestDraw(false);
  }, [data, requestDraw]);

  useEffect(() => {
    uiRef.current = {
      selectedId,
      hoverId,
      query,
      hiddenGroups: new Set(hiddenGroups),
    };
    requestDraw(false);
  }, [selectedId, hoverId, query, hiddenGroups, requestDraw]);

  // ── Canvas sizing / visibility / wheel zoom ───────────────────────────────

  useEffect(() => {
    const container = containerRef.current;
    const canvas = canvasRef.current;
    if (!container || !canvas) return;
    ctxRef.current = canvas.getContext("2d");
    const apply = () => {
      const rect = container.getBoundingClientRect();
      const dpr = window.devicePixelRatio || 1;
      const w = Math.max(1, Math.floor(rect.width));
      const h = Math.max(1, Math.floor(rect.height));
      sizeRef.current = { w, h, dpr };
      canvas.width = Math.max(1, Math.floor(w * dpr));
      canvas.height = Math.max(1, Math.floor(h * dpr));
      canvas.style.width = `${w}px`;
      canvas.style.height = `${h}px`;
      const view = viewRef.current;
      if (view.tx === 0 && view.ty === 0) {
        view.tx = w / 2;
        view.ty = h / 2;
      }
      dirtyRef.current = true;
      ensureLoop();
    };
    apply();
    const ro = new ResizeObserver(apply);
    ro.observe(container);
    window.addEventListener("resize", apply);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", apply);
    };
  }, [ensureLoop]);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    // Pause the render loop while the page is off-screen (IntersectionObserver)
    // or the tab is hidden; the browser throttles rAF for hidden tabs too.
    const io = new IntersectionObserver(
      (entries) => {
        const entry = entries[0];
        visibleRef.current = entry ? entry.isIntersecting : true;
        if (visibleRef.current) {
          dirtyRef.current = true;
          ensureLoop();
        }
      },
      { threshold: 0.01 },
    );
    io.observe(container);
    return () => {
      io.disconnect();
      if (rafRef.current !== 0) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = 0;
      }
    };
  }, [ensureLoop]);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      const rect = canvas.getBoundingClientRect();
      const mx = event.clientX - rect.left;
      const my = event.clientY - rect.top;
      const view = viewRef.current;
      const next = clamp(view.scale * Math.exp(-event.deltaY * 0.0012), 0.12, 4);
      const k = next / view.scale;
      view.tx = mx - (mx - view.tx) * k;
      view.ty = my - (my - view.ty) * k;
      view.scale = next;
      dirtyRef.current = true;
      ensureLoop();
    };
    canvas.addEventListener("wheel", onWheel, { passive: false });
    return () => canvas.removeEventListener("wheel", onWheel);
  }, [ensureLoop]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") clearSelection();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [clearSelection]);

  // ── Derived view data ─────────────────────────────────────────────────────

  const groups = useMemo(() => {
    if (!data) return [] as Array<[string, number]>;
    const counts = new Map<string, number>();
    for (const n of data.nodes) counts.set(n.group, (counts.get(n.group) ?? 0) + 1);
    return [...counts.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1));
  }, [data]);

  const selectedNode = useMemo(
    () => (data && selectedId ? (data.nodes.find((n) => n.id === selectedId) ?? null) : null),
    [data, selectedId],
  );

  const importsOfSelected = useMemo(() => {
    if (!data || !selectedId) return [] as string[];
    return data.links
      .filter((l) => l.source === selectedId)
      .map((l) => l.target)
      .sort();
  }, [data, selectedId]);

  const importedBySelected = useMemo(() => {
    if (!data || !selectedId) return [] as string[];
    return data.links
      .filter((l) => l.target === selectedId)
      .map((l) => l.source)
      .sort();
  }, [data, selectedId]);

  return (
    <div ref={containerRef} style={styles.shell}>
      <canvas
        ref={canvasRef}
        style={{ ...styles.canvas, cursor: hoverId ? "pointer" : "grab" }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onPointerLeave={onPointerLeave}
        role="img"
        aria-label="Interactive code dependency graph"
      />

      <div style={styles.topBar}>
        <Link href="/" style={styles.backLink} aria-label="Back to landing page">
          ←
        </Link>
        <span style={styles.title}>Code graph</span>
        <input
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={onSearchKeyDown}
          placeholder="Search files… (Enter to jump)"
          style={styles.search}
          aria-label="Search files"
          spellCheck={false}
        />
        <span style={styles.meta}>
          {data
            ? `${data.nodes.length} files · ${data.links.length} imports`
            : error
              ? "graph.json unavailable"
              : "loading…"}
        </span>
      </div>

      {error && (
        <div style={styles.error}>
          Could not load /graph.json ({error}). Run <code>npm run graph:gen</code> to
          generate it.
        </div>
      )}
      {!data && !error && <div style={styles.loading}>building graph…</div>}

      {data && (
        <div style={styles.legend}>
          {groups.map(([group, count]) => {
            const hidden = hiddenGroups.includes(group);
            return (
              <button
                key={group}
                type="button"
                onClick={() => toggleGroup(group)}
                style={{ ...styles.legendItem, opacity: hidden ? 0.35 : 1 }}
                title={hidden ? `Show ${group}` : `Hide ${group}`}
              >
                <span style={{ ...styles.dot, background: groupColor(group) }} />
                <span>{group}</span>
                <span style={styles.legendCount}>{count}</span>
              </button>
            );
          })}
        </div>
      )}

      {selectedNode && (
        <aside style={styles.panel}>
          <div style={styles.panelHeader}>
            <div style={{ minWidth: 0 }}>
              <div style={styles.panelTitle}>{selectedNode.label}</div>
              <div style={styles.panelPath}>{selectedNode.id}</div>
            </div>
            <button
              type="button"
              onClick={clearSelection}
              style={styles.panelClose}
              aria-label="Close panel"
            >
              ×
            </button>
          </div>

          <div style={styles.panelMetaRow}>
            <span
              style={{
                ...styles.badge,
                color: groupColor(selectedNode.group),
                borderColor: groupColor(selectedNode.group),
              }}
            >
              {selectedNode.group}
            </span>
            <span style={styles.panelMeta}>{selectedNode.size} LOC</span>
            <span style={styles.panelMeta}>
              {importsOfSelected.length} out · {importedBySelected.length} in
            </span>
          </div>

          <div style={styles.section}>
            <div style={styles.sectionTitle}>Imports ({importsOfSelected.length})</div>
            {importsOfSelected.length === 0 && (
              <div style={styles.empty}>none (external only)</div>
            )}
            {importsOfSelected.map((id) => (
              <button
                key={id}
                type="button"
                onClick={() => jumpTo(id)}
                style={styles.panelItem}
              >
                {id}
              </button>
            ))}
          </div>

          <div style={styles.section}>
            <div style={styles.sectionTitle}>
              Imported by ({importedBySelected.length})
            </div>
            {importedBySelected.length === 0 && <div style={styles.empty}>none</div>}
            {importedBySelected.map((id) => (
              <button
                key={id}
                type="button"
                onClick={() => jumpTo(id)}
                style={styles.panelItem}
              >
                {id}
              </button>
            ))}
          </div>

          <div style={styles.hint}>
            scroll to zoom · drag background to pan · click a node to pin it · Esc to
            release
          </div>
        </aside>
      )}
    </div>
  );
}
