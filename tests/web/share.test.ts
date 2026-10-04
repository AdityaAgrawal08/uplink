import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import type { FakeDb, FakeDoc } from "./helpers/fake-mongo";
import { NextRequest } from "next/server";
import fs from "fs";
import path from "path";

// All share-route tests run against an in-memory Mongo stand-in
// (helpers/fake-mongo.ts) plus MockRedis. `after` must be stubbed: Next.js
// throws "after was called outside a request scope" when invoked outside a
// real request context, and the share routes schedule background cleanup
// through it on the success paths.
const mongo = vi.hoisted(() => ({ handle: null as unknown }));
vi.mock("@/lib/mongodb", async () => {
  const { createFakeMongo } = await import("./helpers/fake-mongo");
  mongo.handle = createFakeMongo();
  return {
    getDb: async () => (mongo.handle as { db: FakeDb }).db,
    initIndexes: async () => {},
  };
});
vi.mock("next/server", async (importOriginal) => {
  const mod = await importOriginal<typeof import("next/server")>();
  return { ...mod, after: vi.fn(() => {}) };
});

import { POST as initPOST } from "../../src/app/api/v1/share/init/route";
import { POST as previewPOST } from "../../src/app/api/v1/share/[id]/preview-text/route";
import { getObjectText, PREVIEW_TEXT_MAX_BYTES } from "../../src/lib/r2";
import { hashPassword } from "../../src/lib/crypto";

function db(): FakeDb {
  return (mongo.handle as { db: FakeDb }).db;
}

function req(url: string, ip: string, body: Record<string, unknown>): NextRequest {
  return new NextRequest(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", "x-forwarded-for": ip },
    body: JSON.stringify(body),
  });
}

const SHA = "a".repeat(64);

async function initShare(ip: string, size: number, extra: Record<string, unknown> = {}) {
  const res = await initPOST(
    req("http://localhost/api/v1/share/init", ip, {
      filename: "f.bin",
      size,
      mimeType: "application/octet-stream",
      hashValue: SHA,
      // multipart so sizes above the 200MB single-part cap are accepted
      partsCount: 5,
      ...extra,
    })
  );
  return res;
}

async function initPOSTOnly(url: string, ip: string, body: Record<string, unknown>) {
  return initPOST(req(url, ip, body));
}

describe("share init: per-IP reservation budget (finding 4)", () => {
  beforeEach(() => {
    db().reset();
    process.env.TRUST_PROXY = "true"; // XFF is the per-IP key in these tests
  });
  afterEach(() => {
    delete process.env.TRUST_PROXY;
  });

  it("same IP: two 500MB inits pass, the third is refused with 429 while another IP succeeds", async () => {
    const MB = 1024 * 1024;
    expect((await initShare("203.0.113.10", 500 * MB)).status).toBe(201);
    expect((await initShare("203.0.113.10", 500 * MB)).status).toBe(201);
    // Exactly at the ~1GB cap the third 500MB is refused.
    const refused = await initShare("203.0.113.10", 500 * MB);
    expect(refused.status).toBe(429);
    expect((await refused.json()).error).toMatch(/uploads in progress/i);
    // A different IP is unaffected.
    expect((await initShare("198.51.100.20", 500 * MB)).status).toBe(201);
  });

  it("small inits from one IP are not throttled", async () => {
    for (let i = 0; i < 5; i++) {
      expect((await initShare("203.0.113.30", 1024)).status).toBe(201);
    }
  });

  it("custom shareId lands on the share doc with initIpHash recorded", async () => {
    const shareId = "My_Custom-ShareId_123456789";
    const res = await initShare("203.0.113.40", 1024, { shareId });
    expect(res.status).toBe(201);
    const share = db().collection("shares").docs.find((d) => d.shareId === shareId) as FakeDoc | undefined;
    expect(share).toBeDefined();
    expect(share!.initIpHash).toBeTruthy();
    expect(share!.size).toBe(1024);
  });
});

describe("preview-text hardening (finding 7)", () => {
  let passwordHash: string;

  beforeEach(async () => {
    db().reset();
    process.env.TRUST_PROXY = "true";
    passwordHash = await hashPassword("hunter2");
  });
  afterEach(() => {
    delete process.env.TRUST_PROXY;
  });

  function seedShare(overrides: Partial<FakeDoc>) {
    db().collection("shares").docs.push({
      shareId: "preview-share-0000001",
      status: "ACTIVE",
      expiresAt: new Date(Date.now() + 3600_000),
      passwordHash: null,
      objectKey: "uploads/2026/10/preview/preview.txt",
      isEncrypted: false,
      downloadsCount: 0,
      downloadLimit: 10,
      ...overrides,
    });
  }

  function previewReq(id: string, ip: string, body: Record<string, unknown>) {
    return previewPOST(
      new NextRequest(`http://localhost/api/v1/share/${id}/preview-text`, {
        method: "POST",
        headers: { "Content-Type": "application/json", "x-forwarded-for": ip },
        body: JSON.stringify(body),
      }),
      { params: Promise.resolve({ id }) }
    );
  }

  it("previews count against downloadsCount/downloadLimit atomically; exhausted → 410", async () => {
    seedShare({ downloadLimit: 2 });
    db().collection("shares").docs[0].downloadsCount = 0;

    const key = "uploads/2026/10/preview/preview.txt";
    const p = path.join(process.cwd(), "uploads_dev", key);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, "hello preview");

    for (let i = 0; i < 2; i++) {
      const res = await previewReq("preview-share-0000001", "203.0.113.50", {});
      expect(res.status).toBe(200);
      expect(((await res.json()) as { text: string }).text).toBe("hello preview");
    }
    // downloadsCount was bumped atomically on each successful preview.
    expect(db().collection("shares").docs[0].downloadsCount).toBe(2);
    // Third preview: limit reached — 410, exactly like authorize-download.
    const exhausted = await previewReq("preview-share-0000001", "203.0.113.50", {});
    expect(exhausted.status).toBe(410);

    fs.rmSync(p, { force: true });
  });

  it("wrong passwords trip the shared failKey lockout: 5 verifies, then 429", async () => {
    seedShare({ passwordHash });
    for (let i = 0; i < 5; i++) {
      const res = await previewReq("preview-share-0000001", "203.0.113.60", { password: "wrong" });
      expect(res.status).toBe(401);
    }
    const locked = await previewReq("preview-share-0000001", "203.0.113.60", { password: "wrong" });
    expect(locked.status).toBe(429);
    // Correct password also refused while locked out (the counter is checked
    // before verification — a lockout is a lockout).
    expect((await previewReq("preview-share-0000001", "203.0.113.60", { password: "hunter2" })).status).toBe(429);
    // A different IP shares no lockout.
    expect((await previewReq("preview-share-0000001", "203.0.113.61", { password: "wrong" })).status).toBe(401);
  });

  it("a successful password resets the lockout (del on success)", async () => {
    seedShare({ passwordHash });
    const key = "uploads/2026/10/preview/preview.txt";
    const p = path.join(process.cwd(), "uploads_dev", key);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, "hello preview");
    try {
      for (let i = 0; i < 3; i++) {
        await previewReq("preview-share-0000001", "203.0.113.62", { password: "wrong" });
      }
      const ok = await previewReq("preview-share-0000001", "203.0.113.62", { password: "hunter2" });
      expect(ok.status).toBe(200);
      // Lockout counter was deleted on success: wrong guesses start fresh.
      const again = await previewReq("preview-share-0000001", "203.0.113.62", { password: "wrong" });
      expect(again.status).toBe(401);
    } finally {
      fs.rmSync(p, { force: true });
    }
  });
});

describe("getObjectText memory cap (finding 7)", () => {
  it("reads at most maxBytes+1 from a large file", async () => {
    const key = `uploads/2026/10/preview/big-${Math.random().toString(36).slice(2)}.txt`;
    const p = path.join(process.cwd(), "uploads_dev", key);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    const content = "A".repeat(200 * 1024); // 200KB — far above the cap
    fs.writeFileSync(p, content);
    try {
      const text = await getObjectText(key);
      expect(text.length).toBeLessThanOrEqual(PREVIEW_TEXT_MAX_BYTES + 1);
      expect(text).toBe(content.slice(0, PREVIEW_TEXT_MAX_BYTES + 1));
    } finally {
      fs.rmSync(p, { force: true });
    }
  });

  it("honors a custom maxBytes (memory cap scales)", async () => {
    const key = `uploads/2026/10/preview/small-${Math.random().toString(36).slice(2)}.txt`;
    const p = path.join(process.cwd(), "uploads_dev", key);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    const content = "B".repeat(10 * 1024);
    fs.writeFileSync(p, content);
    try {
      const text = await getObjectText(key, 100);
      expect(text).toBe("B".repeat(101)); // maxBytes + 1 truncation probe
    } finally {
      fs.rmSync(p, { force: true });
    }
  });

  it("returns empty for missing objects", async () => {
    expect(await getObjectText(`uploads/2026/10/preview/nope-${Math.random().toString(36).slice(2)}.txt`)).toBe("");
  });
});