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
import { POST as authorizePOST } from "../../src/app/api/v1/share/[id]/authorize-download/route";
import { getObjectText, PREVIEW_TEXT_MAX_BYTES } from "../../src/lib/r2";
import * as cryptoMod from "../../src/lib/crypto";
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

describe("authorize-download lockout race (finding 9)", () => {
  let passwordHash: string;

  beforeEach(async () => {
    db().reset();
    process.env.TRUST_PROXY = "true";
    passwordHash = await hashPassword("hunter2");
  });
  afterEach(() => {
    delete process.env.TRUST_PROXY;
    vi.restoreAllMocks();
  });

  function seedShare(overrides: Partial<FakeDoc> = {}) {
    db().collection("shares").docs.push({
      shareId: "auth-share-00000001",
      status: "ACTIVE",
      expiresAt: new Date(Date.now() + 3600_000),
      passwordHash: null,
      objectKey: "uploads/2026/10/auth/auth.bin",
      storageFilename: "auth.bin",
      mimeType: "application/octet-stream",
      downloadsCount: 0,
      downloadLimit: 100,
      ...overrides,
    });
  }

  function authReq(id: string, ip: string, body: Record<string, unknown>) {
    return authorizePOST(
      new NextRequest(`http://localhost/api/v1/share/${id}/authorize-download`, {
        method: "POST",
        headers: { "Content-Type": "application/json", "x-forwarded-for": ip },
        body: JSON.stringify(body),
      }),
      { params: Promise.resolve({ id }) }
    );
  }

  it("parallel wrong passwords verify at most 5 times; the rest 429", async () => {
    seedShare({ passwordHash });
    // Slow the verifier down so 10 truly-concurrent requests overlap: the
    // first 5 increment the counter and enter verify; requests 6-10 see the
    // counter already >5 and are refused WITHOUT verifying.
    const verifySpy = vi.spyOn(cryptoMod, "verifyPassword").mockImplementation(async () => {
      await new Promise((r) => setTimeout(r, 25));
      return false;
    });

    const results = await Promise.all(
      Array.from({ length: 10 }, () => authReq("auth-share-00000001", "203.0.113.70", { password: "wrong" }))
    );
    const statuses = Object.groupBy(results, (r) => r.status);
    expect(statuses[401]?.length ?? 0).toBe(5);
    expect(statuses[429]?.length ?? 0).toBe(5);
    expect(verifySpy).toHaveBeenCalledTimes(5); // bounded: 5 verifies max
    // Still locked for this IP+share afterwards.
    expect((await authReq("auth-share-00000001", "203.0.113.70", { password: "wrong" })).status).toBe(429);
    expect(verifySpy).toHaveBeenCalledTimes(5);
  });

  it("sequential wrong passwords: 5 verifies then lockout", async () => {
    seedShare({ passwordHash });
    for (let i = 0; i < 5; i++) {
      expect((await authReq("auth-share-00000001", "203.0.113.71", { password: "wrong" })).status).toBe(401);
    }
    expect((await authReq("auth-share-00000001", "203.0.113.71", { password: "wrong" })).status).toBe(429);
    // Per-IP isolation: another IP still gets exactly 5 fresh tries.
    expect((await authReq("auth-share-00000001", "203.0.113.72", { password: "wrong" })).status).toBe(401);
  });

  it("correct password deletes the failure counter (del on success)", async () => {
    seedShare({ passwordHash });
    for (let i = 0; i < 3; i++) {
      await authReq("auth-share-00000001", "203.0.113.73", { password: "wrong" });
    }
    const ok = await authReq("auth-share-00000001", "203.0.113.73", { password: "hunter2" });
    expect(ok.status).toBe(200);
    // Counter gone: the next wrong guess starts fresh (401, not 429).
    expect((await authReq("auth-share-00000001", "203.0.113.73", { password: "wrong" })).status).toBe(401);
  });

  it("public shares never touch the failure counter (B42 preserved)", async () => {
    seedShare(); // no passwordHash
    const verifySpy = vi.spyOn(cryptoMod, "verifyPassword").mockImplementation(async () => true);
    const results = await Promise.all(
      Array.from({ length: 10 }, () => authReq("auth-share-00000001", "203.0.113.74", {}))
    );
    for (const r of results) expect(r.status).toBe(200);
    expect(verifySpy).not.toHaveBeenCalled();
  });

  it("preview-text and authorize-download share one lockout budget", async () => {
    seedShare({ passwordHash });
    // Burn 3 guesses through the preview endpoint…
    for (let i = 0; i < 3; i++) {
      const res = await previewPOST(
        new NextRequest(`http://localhost/api/v1/share/auth-share-00000001/preview-text`, {
          method: "POST",
          headers: { "Content-Type": "application/json", "x-forwarded-for": "203.0.113.75" },
          body: JSON.stringify({ password: "wrong" }),
        }),
        { params: Promise.resolve({ id: "auth-share-00000001" }) }
      );
      expect(res.status).toBe(401);
    }
    // …and the 4th guess through authorize-download trips the shared counter
    // to 4, the 5th to 5, and the 6th is locked out.
    expect((await authReq("auth-share-00000001", "203.0.113.75", { password: "wrong" })).status).toBe(401);
    expect((await authReq("auth-share-00000001", "203.0.113.75", { password: "wrong" })).status).toBe(401);
    expect((await authReq("auth-share-00000001", "203.0.113.75", { password: "wrong" })).status).toBe(429);
  });
});

describe("cleanup route auth (finding 11)", () => {
  beforeEach(() => {
    db().reset();
    delete process.env.ADMIN_API_KEY;
    delete process.env.CRON_SECRET;
  });
  afterEach(() => {
    delete process.env.ADMIN_API_KEY;
    delete process.env.CRON_SECRET;
  });

  function cleanupReq(authHeader?: string) {
    const headers: Record<string, string> = {};
    if (authHeader) headers.authorization = authHeader;
    return import("../../src/app/api/v1/cleanup/route").then(({ POST }) =>
      POST(new NextRequest("http://localhost/api/v1/cleanup", { method: "POST", headers }))
    );
  }

  it("unauthenticated cleanup is refused with 401", async () => {
    expect((await cleanupReq()).status).toBe(401);
    expect((await cleanupReq("Bearer wrong-secret")).status).toBe(401);
    expect((await cleanupReq("Basic abc")).status).toBe(401);
  });

  it("ADMIN_API_KEY bearer authorizes the run", async () => {
    process.env.ADMIN_API_KEY = "test-admin-key-123";
    const res = await cleanupReq("Bearer test-admin-key-123");
    expect(res.status).toBe(200);
    expect(((await res.json()) as { message: string }).message).toMatch(/cleanup/i);
  });

  it("CRON_SECRET bearer authorizes the run (Vercel cron style)", async () => {
    process.env.CRON_SECRET = "cron-secret-456";
    const res = await cleanupReq("Bearer cron-secret-456");
    expect(res.status).toBe(200);
  });

  it("a wrong CRON_SECRET is refused even when one is configured", async () => {
    process.env.CRON_SECRET = "cron-secret-456";
    expect((await cleanupReq("Bearer cron-secret-999")).status).toBe(401);
    expect((await cleanupReq("Bearer cron-secret-456-extra")).status).toBe(401); // length mismatch
  });

  it("authorized cleanup processes an expired share end to end", async () => {
    process.env.ADMIN_API_KEY = "test-admin-key-123";
    db().collection("shares").docs.push({
      shareId: "cleanup-me-00000001",
      status: "ACTIVE",
      expiresAt: new Date(Date.now() - 1000),
      objectKey: "uploads/2026/10/cleanup/gone.bin",
      size: 1234,
    });
    const res = await cleanupReq("Bearer test-admin-key-123");
    expect(res.status).toBe(200);
    const body = (await res.json()) as { actions: Array<{ shareId: string; status: string }> };
    expect(body.actions.find((a) => a.shareId === "cleanup-me-00000001")?.status).toBe("DELETED");
    const doc = db().collection("shares").docs.find((d) => d.shareId === "cleanup-me-00000001");
    expect(doc?.status).toBe("DELETED");
  });

  it("internal performCleanup stays directly callable (after()-hook path)", async () => {
    // Share routes call performCleanup straight from their after() hooks —
    // that internal path must keep working without any credential.
    const { performCleanup } = await import("../../src/app/api/v1/cleanup/route");
    const res = await performCleanup();
    expect(res.status).toBe(200);
  });
});