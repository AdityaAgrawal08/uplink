import { describe, it, expect, beforeEach, vi } from "vitest";
import type { FakeDb } from "./helpers/fake-mongo";

// All quota/lib tests in this file run against an in-memory Mongo stand-in
// (see helpers/fake-mongo.ts) that evaluates the exact filter/update
// expressions the quota pipeline sends.
const mongo = vi.hoisted(() => ({ handle: null as unknown }));
vi.mock("@/lib/mongodb", async () => {
  const { createFakeMongo } = await import("./helpers/fake-mongo");
  mongo.handle = createFakeMongo();
  return {
    getDb: async () => (mongo.handle as { db: FakeDb }).db,
    initIndexes: async () => {},
  };
});

import {
  reserveUploadQuota,
  commitUploadQuota,
  releaseUploadQuota,
  releaseUploadQuotaWithRetry,
  getQuotaState,
} from "../../src/lib/quota";

function seedQuota(db: FakeDb, overrides: Record<string, unknown> = {}) {
  const now = new Date();
  const nextMonth = new Date(now);
  nextMonth.setUTCMonth(nextMonth.getUTCMonth() + 1);
  nextMonth.setUTCDate(1);
  nextMonth.setUTCHours(0, 0, 0, 0);
  const nextDay = new Date(now);
  nextDay.setUTCDate(nextDay.getUTCDate() + 1);
  nextDay.setUTCHours(0, 0, 0, 0);
  db.collection("quotas").docs.push({
    _id: "r2_quota",
    storageBytes: 0,
    reservedBytes: 0,
    classAOps: 0,
    classBOps: 0,
    classAResetAt: nextMonth,
    classBResetAt: nextDay,
    quotaEvents: [],
    ...overrides,
  });
}

describe("quota release (double-release TOCTOU)", () => {
  let db: FakeDb;

  beforeEach(() => {
    db = (mongo.handle as { db: FakeDb }).db;
    db.reset();
    seedQuota(db, { reservedBytes: 1000, classAOps: 10, storageBytes: 5000 });
  });

  it("single release refunds the reservation", async () => {
    await releaseUploadQuota(600, 5);
    const state = await getQuotaState();
    expect(state.reservedBytes).toBe(400);
    expect(state.classAOps).toBe(5);
    expect(state.storageBytes).toBe(5000);
  });

  it("concurrent double-release never drives reservedBytes negative", async () => {
    // Two releases of the same reservation race (the audit scenario: a
    // confirm error path and the cleanup sweep both refund). The pipeline's
    // $max floor clamps the second refund to 0 — the ledger must read 0, not
    // -200, and storageBytes must be untouched.
    await Promise.all([releaseUploadQuota(600, 5), releaseUploadQuota(600, 5)]);
    const state = await getQuotaState();
    expect(state.reservedBytes).toBe(0);
    expect(state.classAOps).toBe(0);
    expect(state.storageBytes).toBe(5000);
  });

  it("release refunding more than reserved clamps at zero, never negative", async () => {
    await releaseUploadQuota(5000, 100); // far above the 1000 reserved
    const state = await getQuotaState();
    expect(state.reservedBytes).toBe(0);
    expect(state.classAOps).toBe(0);
  });

  it("release then commit stays consistent (no double booking)", async () => {
    await releaseUploadQuota(600, 5);
    await releaseUploadQuota(600, 5); // second is a no-op
    await commitUploadQuota(400); // what was actually uploaded
    const state = await getQuotaState();
    expect(state.reservedBytes).toBe(0);
    expect(state.storageBytes).toBe(5400);
  });

  it("releaseUploadQuotaWithRetry succeeds on the happy path and returns true", async () => {
    const ok = await releaseUploadQuotaWithRetry(600, 5);
    expect(ok).toBe(true);
    expect((await getQuotaState()).reservedBytes).toBe(400);
  });

  it("reserve/commit/release round-trip keeps the ledger in balance", async () => {
    db.reset();
    seedQuota(db);
    expect(await reserveUploadQuota(2048, 1)).toBe(true);
    let state = await getQuotaState();
    expect(state.reservedBytes).toBe(2048);
    await commitUploadQuota(2048);
    state = await getQuotaState();
    expect(state.reservedBytes).toBe(0);
    expect(state.storageBytes).toBe(2048);
    await releaseUploadQuota(2048, 1); // stale double-release: no-op
    state = await getQuotaState();
    expect(state.reservedBytes).toBe(0);
  });
});