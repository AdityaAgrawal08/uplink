import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import type { FakeDb, FakeDoc } from "./helpers/fake-mongo";
import { NextRequest } from "next/server";

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