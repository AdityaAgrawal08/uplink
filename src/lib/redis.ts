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
  hgetall(key: string): Promise<Record<string, string> | null>;
  hdel(key: string, ...fields: string[]): Promise<number>;
  hlen(key: string): Promise<number>;
  // List ops (signaling queues).
  rpush(key: string, ...values: string[]): Promise<number>;
  lrange(key: string, start: number, stop: number): Promise<string[]>;
  ltrim(key: string, start: number, stop: number): Promise<string>;
  llen(key: string): Promise<number>;
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
    if (item) {
      if (item.expiry && Date.now() > item.expiry) {
        this.store.delete(key);
      } else {
        val = parseInt(item.value as string, 10);
        if (isNaN(val)) val = 0;
      }
    }
    val += 1;
    this.store.set(key, { value: String(val), expiry: item?.expiry || null, isObject: false });
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
}

class LazyRedisClient implements IRedisClient {
  private client: IRedisClient | null = null;
  private isFallbackToMock = false;

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
    } else {
      if (this.isFallbackToMock) {
        console.warn("Upstash Redis connection failed or unreachable. Falling back to local in-memory MockRedis.");
      } else if (!redisUrl || !redisToken || isPlaceholder(redisUrl) || isPlaceholder(redisToken)) {
        // silent fallback for local/dev without credentials
        this.client = new MockRedis();
        return this.client;
      } else {
        console.log("Upstash Redis credentials missing. Using local in-memory MockRedis.");
      }
      this.client = new MockRedis();
    }
    return this.client;
  }

  private async executeWithFallback<T>(operation: (client: IRedisClient) => Promise<T>): Promise<T> {
    try {
      return await operation(this.getClient());
    } catch (err) {
      if (!this.isFallbackToMock) {
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
    return this.executeWithFallback(c => c.hset(key, field, value));
  }

  async hgetall(key: string): Promise<Record<string, string> | null> {
    return this.executeWithFallback(c => c.hgetall(key));
  }

  async hdel(key: string, ...fields: string[]): Promise<number> {
    return this.executeWithFallback(c => c.hdel(key, ...fields));
  }

  async hlen(key: string): Promise<number> {
    return this.executeWithFallback(c => c.hlen(key));
  }

  async rpush(key: string, ...values: string[]): Promise<number> {
    return this.executeWithFallback(c => c.rpush(key, ...values));
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
}

export const redis = new LazyRedisClient();
