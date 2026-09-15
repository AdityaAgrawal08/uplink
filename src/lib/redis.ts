import { Redis } from "@upstash/redis";

export interface IRedisClient {
  get(key: string): Promise<unknown>;
  set(key: string, value: unknown, options?: { ex?: number; px?: number; nx?: boolean }): Promise<unknown>;
  incr(key: string): Promise<number>;
  expire(key: string, seconds: number): Promise<number>;
  del(key: string): Promise<number>;
  // Hash ops (rooms, members, inboxes). All values are strings.
  hsetnx(key: string, field: string, value: string): Promise<number>;
  hset(key: string, field: string, value: string): Promise<number>;
  hget(key: string, field: string): Promise<string | null>;
  hgetall(key: string): Promise<Record<string, string> | null>;
  hdel(key: string, ...fields: string[]): Promise<number>;
  hlen(key: string): Promise<number>;
  // List ops (signaling queues).
  rpush(key: string, ...values: string[]): Promise<number>;
  rpop(key: string): Promise<string | null>;
  lrem(key: string, count: number, value: string): Promise<number>;
  lrange(key: string, start: number, stop: number): Promise<string[]>;
  ltrim(key: string, start: number, stop: number): Promise<string>;
  llen(key: string): Promise<number>;
  pipeline(ops: PipeOp[]): Promise<unknown[]>;
}

// One pipelined operation: Upstash executes the batch in a single round
// trip (verified: 3 ops in ~300ms vs ~500ms+ sequential at 170ms RTT).
// Results come back positionally; a failing command throws (fail-fast),
// matching sequential semantics where the error would surface inline.
export interface PipeOp {
  cmd: "hsetnx" | "hset" | "hget" | "expire" | "rpush" | "rpop" | "lrem" | "ltrim" | "del" | "hgetall" | "hdel" | "lrange" | "llen" | "hlen" | "incr";
  key: string;
  args: Array<string | number>;
}

// Exported for unit tests (and available as a last-resort embeddable store).
export class MockRedis implements IRedisClient {
  private store: Map<string, { value: unknown; expiry: number | null; isObject: boolean }> = new Map();
  private hashes: Map<string, { fields: Map<string, string>; expiry: number | null }> = new Map();
  private lists: Map<string, { items: string[]; expiry: number | null }> = new Map();

  // Drop the key if its TTL passed. Returns true when the key is gone.
  private expired(map: Map<string, { expiry: number | null }>, key: string): boolean {
    const item = map.get(key);
    if (!item) return true;
    if (item.expiry && Date.now() > item.expiry) {
      map.delete(key);
      return true;
    }
    return false;
  }

  async get(key: string): Promise<unknown> {
    const item = this.store.get(key);
    if (!item) return null;
    if (item.expiry && Date.now() > item.expiry) {
      this.store.delete(key);
      return null;
    }
    if (item.isObject && typeof item.value === "string") {
      try {
        return JSON.parse(item.value);
      } catch {
        return item.value;
      }
    }
    return item.value;
  }

  async set(key: string, value: unknown, options?: { ex?: number; px?: number; nx?: boolean }): Promise<unknown> {
    if (options?.nx) {
      const existing = await this.get(key);
      if (existing !== null) {
        return null;
      }
    }

    let expiry: number | null = null;
    if (options?.ex) {
      expiry = Date.now() + options.ex * 1000;
    } else if (options?.px) {
      expiry = Date.now() + options.px;
    }

    const isObject = typeof value === "object" && value !== null;
    const valueToStore = isObject ? JSON.stringify(value) : value;
    this.store.set(key, { value: valueToStore, expiry, isObject });
    return "OK";
  }

  async incr(key: string): Promise<number> {
    const item = this.store.get(key);
    let val = 0;
    let expiry: number | null = null;
    if (item) {
      if (item.expiry && Date.now() > item.expiry) {
        this.store.delete(key); // window rolled: fresh counter, fresh TTL
      } else {
        val = parseInt(item.value as string, 10);
        if (isNaN(val)) val = 0;
        expiry = item.expiry;
      }
    }
    val += 1;
    this.store.set(key, { value: String(val), expiry, isObject: false });
    return val;
  }

  async expire(key: string, seconds: number): Promise<number> {
    // TTLs apply to plain keys, hashes, and lists alike (room sliding expiry
    // refreshes member/signal/inbox keys together).
    let found = 0;
    const expiry = Date.now() + seconds * 1000;
    const item = this.store.get(key);
    if (item) {
      item.expiry = expiry;
      found = 1;
    }
    const h = this.hashes.get(key);
    if (h) {
      h.expiry = expiry;
      found = 1;
    }
    const l = this.lists.get(key);
    if (l) {
      l.expiry = expiry;
      found = 1;
    }
    return found;
  }

  async del(key: string): Promise<number> {
    let n = 0;
    if (this.store.delete(key)) n++;
    if (this.hashes.delete(key)) n++;
    if (this.lists.delete(key)) n++;
    return n;
  }

  // ---- hashes ----

  async hsetnx(key: string, field: string, value: string): Promise<number> {
    if (this.expired(this.hashes, key)) {
      this.hashes.set(key, { fields: new Map(), expiry: null });
    }
    const h = this.hashes.get(key)!;
    if (h.fields.has(field)) return 0;
    h.fields.set(field, value);
    return 1;
  }

  async hset(key: string, field: string, value: string): Promise<number> {
    if (this.expired(this.hashes, key)) {
      this.hashes.set(key, { fields: new Map(), expiry: null });
    }
    const h = this.hashes.get(key)!;
    const isNew = h.fields.has(field) ? 0 : 1;
    h.fields.set(field, value);
    return isNew;
  }

  async hgetall(key: string): Promise<Record<string, string> | null> {
    if (this.expired(this.hashes, key)) return null;
    const h = this.hashes.get(key)!;
    if (h.fields.size === 0) return null;
    const out: Record<string, string> = {};
    for (const [f, v] of h.fields) out[f] = v;
    return out;
  }

  async hdel(key: string, ...fields: string[]): Promise<number> {
    if (this.expired(this.hashes, key)) return 0;
    const h = this.hashes.get(key)!;
    let n = 0;
    for (const f of fields) {
      if (h.fields.delete(f)) n++;
    }
    return n;
  }

  async hlen(key: string): Promise<number> {
    if (this.expired(this.hashes, key)) return 0;
    return this.hashes.get(key)!.fields.size;
  }

  async hget(key: string, field: string): Promise<string | null> {
    if (this.expired(this.hashes, key)) return null;
    return this.hashes.get(key)!.fields.get(field) ?? null;
  }

  // ---- lists ----

  async rpush(key: string, ...values: string[]): Promise<number> {
    if (this.expired(this.lists, key)) {
      this.lists.set(key, { items: [], expiry: null });
    }
    const l = this.lists.get(key)!;
    l.items.push(...values);
    return l.items.length;
  }

  async lrange(key: string, start: number, stop: number): Promise<string[]> {
    if (this.expired(this.lists, key)) return [];
    const items = this.lists.get(key)!.items;
    const end = stop < 0 ? items.length + stop + 1 : stop + 1;
    return items.slice(Math.max(0, start), Math.max(0, end));
  }

  async rpop(key: string): Promise<string | null> {
    if (this.expired(this.lists, key)) return null;
    return this.lists.get(key)!.items.pop() ?? null;
  }

  async lrem(key: string, count: number, value: string): Promise<number> {
    if (this.expired(this.lists, key)) return 0;
    const items = this.lists.get(key)!.items;
    let removed = 0;
    if (count >= 0) {
      for (let i = items.length - 1; i >= 0 && (count === 0 || removed < count); i--) {
        if (items[i] === value) {
          items.splice(i, 1);
          removed++;
        }
      }
    } else {
      for (let i = 0; i < items.length && removed < -count; ) {
        if (items[i] === value) {
          items.splice(i, 1);
          removed++;
        } else {
          i++;
        }
      }
    }
    return removed;
  }

  async ltrim(key: string, start: number, stop: number): Promise<string> {
    if (this.expired(this.lists, key)) return "OK";
    const l = this.lists.get(key)!;
    const end = stop < 0 ? l.items.length + stop + 1 : stop + 1;
    l.items = l.items.slice(Math.max(0, start), Math.max(0, end));
    return "OK";
  }

  async llen(key: string): Promise<number> {
    if (this.expired(this.lists, key)) return 0;
    return this.lists.get(key)!.items.length;
  }

  // Sequential execution with identical ordering/error semantics to a
  // server-side pipeline (errors thrown by an op abort the batch, mirroring
  // Upstash exec fail-fast so both backends behave alike).
  async pipeline(ops: PipeOp[]): Promise<unknown[]> {
    const out: unknown[] = [];
    for (const op of ops) {
      switch (op.cmd) {
        case "hsetnx": out.push(await this.hsetnx(op.key, String(op.args[0]), String(op.args[1]))); break;
        case "hset": out.push(await this.hset(op.key, String(op.args[0]), String(op.args[1]))); break;
        case "hget": out.push(await this.hget(op.key, String(op.args[0]))); break;
        case "expire": out.push(await this.expire(op.key, Number(op.args[0]))); break;
        case "rpush": out.push(await this.rpush(op.key, ...op.args.map(String))); break;
        case "rpop": out.push(await this.rpop(op.key)); break;
        case "lrem": out.push(await this.lrem(op.key, Number(op.args[0]), String(op.args[1]))); break;
        case "ltrim": out.push(await this.ltrim(op.key, Number(op.args[0]), Number(op.args[1]))); break;
        case "del": out.push(await this.del(op.key)); break;
        case "hgetall": out.push(await this.hgetall(op.key)); break;
        case "hdel": out.push(await this.hdel(op.key, ...op.args.map(String))); break;
        case "lrange": out.push(await this.lrange(op.key, Number(op.args[0]), Number(op.args[1]))); break;
        case "llen": out.push(await this.llen(op.key)); break;
        case "hlen": out.push(await this.hlen(op.key)); break;
        case "incr": out.push(await this.incr(op.key)); break;
        default: throw new Error(`unsupported pipeline op: ${op.cmd}`);
      }
    }
    return out;
  }
}

// getDevMockRedis returns a process-wide MockRedis in development, cached
// on globalThis so Next.js dev hot-reloads (which re-execute route modules)
// don't silently reset signaling state mid-session. Same convention as the
// Mongo client cache in mongodb.ts. Production never reaches here without
// ALLOW_MOCK_REDIS (fail-loud above), and serverless isolates stay separate
// by design — shared state there comes only from Upstash.
function getDevMockRedis(): MockRedis {
  const g = globalThis as typeof globalThis & { __uplinkMockRedis?: MockRedis };
  if (!g.__uplinkMockRedis) {
    g.__uplinkMockRedis = new MockRedis();
  }
  return g.__uplinkMockRedis;
}

export class LazyRedisClient implements IRedisClient {
  private client: IRedisClient | null = null;
  private isFallbackToMock = false;
  private fallbackWarned = false;

  // Production must never silently serve split-brain state: without a
  // shared backend, every serverless isolate gets its own memory, so rooms
  // created on one request 404 on the next. Fail loud (503) unless the
  // operator explicitly opts into mock mode (CI does: ALLOW_MOCK_REDIS).
  private mockAllowed(): boolean {
    if (process.env.NODE_ENV !== "production") return true;
    return process.env.ALLOW_MOCK_REDIS === "true";
  }

  private warnFallbackOnce(err: unknown): void {
    if (this.fallbackWarned) return;
    this.fallbackWarned = true;
    console.warn(
      "Upstash Redis unreachable and no usable fallback: signaling is DOWN. " +
      "Set UPSTASH_REDIS_REST_URL/TOKEN (and redeploy), or ALLOW_MOCK_REDIS=true " +
      "only for throwaway/CI environments — mock mode split-brains across instances."
    );
    console.warn(err);
  }

  private getClient(): IRedisClient {
    if (this.client) return this.client;

    const redisUrl = process.env.UPSTASH_REDIS_REST_URL;
    const redisToken = process.env.UPSTASH_REDIS_REST_TOKEN;

    const isPlaceholder = (v?: string) => !v || /placeholder|example|_here|xxxx/i.test(v) || v.includes("bad port");

    if (redisUrl && redisToken && !isPlaceholder(redisUrl) && !isPlaceholder(redisToken) && !this.isFallbackToMock) {
      this.client = new Redis({
        url: redisUrl,
        token: redisToken,
      }) as unknown as IRedisClient;
      return this.client;
    }

    // No usable backend: MockRedis only where it cannot split-brain.
    if (!this.mockAllowed()) {
      throw new Error(
        "No Redis backend configured (set UPSTASH_REDIS_REST_URL/TOKEN) and " +
        "ALLOW_MOCK_REDIS is not enabled. Refusing MockRedis in production: " +
        "per-instance memory would split-brain rooms across serverless isolates."
      );
    }
    if (!this.isFallbackToMock) {
      // silent in dev without credentials; loud once after a real failure
      console.log("Using local in-memory MockRedis (no usable Upstash credentials).");
    }
    this.client = getDevMockRedis();
    return this.client;
  }

  private async executeWithFallback<T>(operation: (client: IRedisClient) => Promise<T>): Promise<T> {
    try {
      return await operation(this.getClient());
    } catch (err) {
      if (!this.isFallbackToMock) {
        if (!this.mockAllowed()) {
          this.warnFallbackOnce(err);
          throw err;
        }
        console.warn("Upstash Redis operation failed. Falling back to local in-memory MockRedis.", err);
        this.isFallbackToMock = true;
        this.client = null; // force re-creation of client as MockRedis
        return await operation(this.getClient());
      }
      throw err;
    }
  }

  async get(key: string): Promise<unknown> {
    return this.executeWithFallback(c => c.get(key));
  }

  async set(key: string, value: unknown, options?: { ex?: number; px?: number; nx?: boolean }): Promise<unknown> {
    return this.executeWithFallback(c => c.set(key, value, options));
  }

  async incr(key: string): Promise<number> {
    return this.executeWithFallback(c => c.incr(key));
  }

  async expire(key: string, seconds: number): Promise<number> {
    return this.executeWithFallback(c => c.expire(key, seconds));
  }

  async del(key: string): Promise<number> {
    return this.executeWithFallback(c => c.del(key));
  }

  async hsetnx(key: string, field: string, value: string): Promise<number> {
    return this.executeWithFallback(c => c.hsetnx(key, field, value));
  }

  async hset(key: string, field: string, value: string): Promise<number> {
    // Upstash v1.38 hset accepts ONLY the object form — hset(key, field,
    // value) silently spreads the field string into indexed garbage fields
    // (verified live: {"0":"a","1":"l",...}). MockRedis takes positional
    // args. Adapt at this boundary so one contract serves both backends.
    const run = async (): Promise<number> => {
      const client = this.getClient();
      if (client instanceof MockRedis) return client.hset(key, field, value);
      const raw = client as unknown as {
        hset(k: string, obj: Record<string, string>): Promise<number>;
      };
      return raw.hset(key, { [field]: value });
    };
    try {
      return await run();
    } catch (err) {
      if (!this.isFallbackToMock && this.mockAllowed()) {
        this.isFallbackToMock = true;
        this.client = null;
        return (this.getClient() as MockRedis).hset(key, field, value);
      }
      throw err;
    }
  }

  async hgetall(key: string): Promise<Record<string, string> | null> {
    return this.executeWithFallback(c => c.hgetall(key));
  }

  async hdel(key: string, ...fields: string[]): Promise<number> {
    return this.executeWithFallback(c => c.hdel(key, ...fields));
  }

  async hget(key: string, field: string): Promise<string | null> {
    return this.executeWithFallback(c => c.hget(key, field));
  }

  async hlen(key: string): Promise<number> {
    return this.executeWithFallback(c => c.hlen(key));
  }

  async rpush(key: string, ...values: string[]): Promise<number> {
    return this.executeWithFallback(c => c.rpush(key, ...values));
  }

  async rpop(key: string): Promise<string | null> {
    return this.executeWithFallback(c => c.rpop(key));
  }

  async lrem(key: string, count: number, value: string): Promise<number> {
    return this.executeWithFallback(c => c.lrem(key, count, value));
  }

  async lrange(key: string, start: number, stop: number): Promise<string[]> {
    return this.executeWithFallback(c => c.lrange(key, start, stop));
  }

  async ltrim(key: string, start: number, stop: number): Promise<string> {
    return this.executeWithFallback(c => c.ltrim(key, start, stop));
  }

  async llen(key: string): Promise<number> {
    return this.executeWithFallback(c => c.llen(key));
  }

  async pipeline(ops: PipeOp[]): Promise<unknown[]> {
    const run = async (): Promise<unknown[]> => {
      const client = this.getClient();
      if (client instanceof MockRedis) return client.pipeline(ops);
      // Real Upstash: one round trip via the pipeline builder. Method
      // names match 1:1 (lowercase); exec fail-fasts like sequential ops.
      const raw = client as unknown as {
        pipeline(): Record<string, (...args: unknown[]) => unknown> & { exec(): Promise<unknown[]> };
      };
      const p = raw.pipeline();
      for (const op of ops) {
        if (op.cmd === "hset") {
          // Object form only (see hset override): positional spreads the
          // field string into indexed garbage fields on real Upstash.
          const [field, value] = op.args;
          (p.hset as unknown as (k: string, o: Record<string, string>) => unknown).call(
            p, op.key, { [String(field)]: String(value) }
          );
          continue;
        }
        const fn = p[op.cmd];
        if (typeof fn !== "function") throw new Error(`unsupported pipeline op: ${op.cmd}`);
        fn.call(p, op.key, ...op.args);
      }
      return p.exec();
    };
    try {
      return await run();
    } catch (err) {
      // Single-request batch: a network failure means nothing ran
      // server-side, so one mock retry is safe (same guarantee as the
      // per-op fallback). Production without opt-in fails loud instead.
      if (!this.isFallbackToMock && this.mockAllowed()) {
        this.isFallbackToMock = true;
        this.client = null;
        return (this.getClient() as MockRedis).pipeline(ops);
      }
      throw err;
    }
  }
}

export const redis = new LazyRedisClient();
