import { Redis } from "@upstash/redis";

export interface IRedisClient {
  get(key: string): Promise<unknown>;
  set(key: string, value: unknown, options?: { ex?: number; px?: number; nx?: boolean }): Promise<unknown>;
  incr(key: string): Promise<number>;
  expire(key: string, seconds: number): Promise<number>;
  del(key: string): Promise<number>;
}

class MockRedis implements IRedisClient {
  private store: Map<string, { value: unknown; expiry: number | null; isObject: boolean }> = new Map();

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
    const item = this.store.get(key);
    if (!item) return 0;
    item.expiry = Date.now() + seconds * 1000;
    return 1;
  }

  async del(key: string): Promise<number> {
    const deleted = this.store.delete(key);
    return deleted ? 1 : 0;
  }
}

class LazyRedisClient implements IRedisClient {
  private client: IRedisClient | null = null;
  private isFallbackToMock = false;

  private getClient(): IRedisClient {
    if (this.client) return this.client;

    const redisUrl = process.env.UPSTASH_REDIS_REST_URL;
    const redisToken = process.env.UPSTASH_REDIS_REST_TOKEN;

    if (redisUrl && redisToken && !this.isFallbackToMock) {
      this.client = new Redis({
        url: redisUrl,
        token: redisToken,
      }) as unknown as IRedisClient;
    } else {
      if (this.isFallbackToMock) {
        console.warn("Upstash Redis connection failed or unreachable. Falling back to local in-memory MockRedis.");
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
}

export const redis = new LazyRedisClient();
