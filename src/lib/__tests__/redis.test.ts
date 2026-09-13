import { describe, it, expect, beforeEach, vi, afterEach } from "vitest";
import { MockRedis, LazyRedisClient } from "../redis";

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

  // B13: Document that SET without EX/PX clears TTL (matches real Redis).
  // This is the correct behavior but can surprise callers who expect TTL
  // preservation across re-sets.
  it("set without expiry clears prior TTL (matches real Redis SET behavior)", async () => {
    await r.set("k", "v1", { ex: 3600 });
    const itemBefore = r["store"].get("k");
    expect(itemBefore?.expiry).not.toBeNull();

    // Overwrite without EX — TTL should be lost.
    await r.set("k", "v2");
    const itemAfter = r["store"].get("k");
    expect(itemAfter?.expiry).toBeNull();
    expect(await r.get("k")).toBe("v2");

    // incr after set-without-TTL should also have no TTL.
    await r.incr("counter");
    await r.expire("counter", 60);
    await r.set("counter", "999"); // overwrite clears TTL
    const counterItem = r["store"].get("counter");
    expect(counterItem?.expiry).toBeNull();
  });

  it("hsetnx claims a field exactly once (atomic username-claim contract)", async () => {
    expect(await r.hsetnx("room:m", "alice", "{}")).toBe(1);
    expect(await r.hsetnx("room:m", "alice", "{}")).toBe(0);
    expect(await r.hsetnx("room:m", "bob", "{}")).toBe(1);
    expect(await r.hlen("room:m")).toBe(2);
    expect(await r.hgetall("room:m")).toEqual({ alice: "{}", bob: "{}" });
    expect(await r.hdel("room:m", "alice")).toBe(1);
    expect(await r.hdel("room:m", "alice")).toBe(0);
    expect(await r.hgetall("room:m")).toEqual({ bob: "{}" });
  });

  it("lists behave as FIFO queues with trim", async () => {
    expect(await r.lrange("q", 0, -1)).toEqual([]);
    await r.rpush("q", "a", "b", "c");
    expect(await r.llen("q")).toBe(3);
    expect(await r.lrange("q", 0, -1)).toEqual(["a", "b", "c"]);
    expect(await r.lrange("q", 1, 2)).toEqual(["b", "c"]);
    await r.ltrim("q", 1, -1);
    expect(await r.lrange("q", 0, -1)).toEqual(["b", "c"]);
  });

  it("expire applies to hashes and lists, del clears all types", async () => {
    await r.hset("h", "f", "v");
    await r.rpush("l", "x");
    expect(await r.expire("h", 3600)).toBe(1);
    expect(await r.expire("l", 3600)).toBe(1);
    expect(await r.expire("missing", 60)).toBe(0);
    expect(await r.del("h")).toBe(1);
    expect(await r.hgetall("h")).toBeNull();
    expect(await r.del("l")).toBe(1);
    expect(await r.llen("l")).toBe(0);
  });

  // Production without a Redis backend must fail LOUD (split-brain rooms
  // across serverless isolates are worse than an outage). MockRedis is
  // only allowed outside production, or with explicit ALLOW_MOCK_REDIS
  // opt-in (CI uses it).
  describe("production backend gating", () => {
    const saved: Record<string, string | undefined> = {};
    beforeEach(() => {
      for (const k of ["NODE_ENV", "UPSTASH_REDIS_REST_URL", "UPSTASH_REDIS_REST_TOKEN", "ALLOW_MOCK_REDIS"]) {
        saved[k] = process.env[k];
        delete process.env[k];
      }
    });
    afterEach(() => {
      for (const [k, v] of Object.entries(saved)) {
        if (v === undefined) delete process.env[k];
        else process.env[k] = v;
      }
      vi.unstubAllEnvs();
    });

    it("throws in production with no backend and no opt-in", async () => {
      vi.stubEnv("NODE_ENV", "production");
      const c = new LazyRedisClient();
      await expect(c.incr("k")).rejects.toThrow(/No Redis backend/);
    });

    it("uses MockRedis in production with explicit opt-in", async () => {
      vi.stubEnv("NODE_ENV", "production");
      vi.stubEnv("ALLOW_MOCK_REDIS", "true");
      const c = new LazyRedisClient();
      await expect(c.incr("k")).resolves.toBe(1);
    });

    it("uses MockRedis outside production without opt-in", async () => {
      vi.stubEnv("NODE_ENV", "development");
      const c = new LazyRedisClient();
      await expect(c.incr("k")).resolves.toBe(1);
    });
  });
});
