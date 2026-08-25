import { describe, it, expect, beforeEach, vi } from "vitest";
import { MockRedis } from "../redis";

// MockRedis mirrors the tiny Redis surface the app relies on. These tests pin
// its semantics so the graceful-degradation path behaves identically to real
// Upstash for: counters+TTL, NX set, object JSON round-trip, expiry sweeps.

describe("MockRedis", () => {
  let r: MockRedis;
  beforeEach(() => { r = new MockRedis(); });

  it("incr starts at 1 and accumulates", async () => {
    expect(await r.incr("hits")).toBe(1);
    expect(await r.incr("hits")).toBe(2);
    expect(await r.incr("other")).toBe(1);
  });

  it("expire then incr keeps TTL (sliding window contract)", async () => {
    await r.incr("k");
    await r.expire("k", 100);
    await r.incr("k");
    // internal expiry must survive the increment
    const item = r["store"].get("k");
    expect(item?.expiry).not.toBeNull();
    // GET returns the raw wire string — identical to real Upstash REST
    // (counters are only ever read through incr's numeric return).
    expect(await r.get("k")).toBe("2");
  });

  it("expired keys read as null", async () => {
    await r.set("tmp", "v", { ex: -1 }); // already in the past
    expect(await r.get("tmp")).toBeNull();
  });

  it("nx refuses overwrite of live key", async () => {
    expect(await r.set("lock", "a", { nx: true })).toBe("OK");
    expect(await r.set("lock", "b", { nx: true })).toBeNull();
    expect(await r.get("lock")).toBe("a");
  });

  it("nx succeeds once a prior key expired", async () => {
    await r.set("lock", "a", { px: -1 });
    expect(await r.set("lock", "b", { nx: true })).toBe("OK");
  });

  it("objects are stored as JSON strings and parsed back", async () => {
    const obj = { shareId: "abc", n: 3 };
    await r.set("o", obj);
    expect(await r.get("o")).toEqual(obj);
  });

  it("del removes exactly one key", async () => {
    await r.set("d", 1);
    expect(await r.del("d")).toBe(1);
    expect(await r.del("d")).toBe(0);
    expect(await r.get("d")).toBeNull();
  });

  it("rate-limit pattern: hits beyond limit within window all see >limit", async () => {
    for (let i = 0; i < 20; i++) {
      const hits = await r.incr("rl");
      if (hits === 1) await r.expire("rl", 10);
      expect(hits).toBe(i + 1);
    }
    expect(await r.incr("rl")).toBe(21); // caller compares > LIMIT
  });
});
