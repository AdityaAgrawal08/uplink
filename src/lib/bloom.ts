import crypto from "crypto";

export class BloomFilter {
  bits: Buffer;
  m: number;
  k: number;

  constructor(m: number, k: number, bits?: Buffer) {
    this.m = m;
    this.k = k;
    this.bits = bits || Buffer.alloc(Math.ceil(m / 8));
  }

  /**
   * Generates k hash indices for a string using double hashing of SHA-256 hash.
   */
  private getIndices(item: string): number[] {
    const hash = crypto.createHash("sha256").update(item).digest();
    const indices: number[] = [];
    const hash1 = hash.readUInt32BE(0);
    const hash2 = hash.readUInt32BE(4);
    for (let i = 0; i < this.k; i++) {
      // Double hashing: (hash1 + i * hash2) % m
      const index = Math.abs((hash1 + i * hash2) % this.m);
      indices.push(index);
    }
    return indices;
  }

  add(item: string): void {
    const indices = this.getIndices(item);
    for (const idx of indices) {
      const byteIdx = Math.floor(idx / 8);
      const bitIdx = idx % 8;
      this.bits[byteIdx] |= (1 << bitIdx);
    }
  }

  contains(item: string): boolean {
    const indices = this.getIndices(item);
    for (const idx of indices) {
      const byteIdx = Math.floor(idx / 8);
      const bitIdx = idx % 8;
      if ((this.bits[byteIdx] & (1 << bitIdx)) === 0) {
        return false;
      }
    }
    return true;
  }

  toJSON() {
    return {
      m: this.m,
      k: this.k,
      bits: this.bits.toString("base64"),
    };
  }

  static fromJSON(json: { m: number; k: number; bits: string | Buffer | { buffer?: unknown; value?: () => Buffer } | unknown }): BloomFilter {
    let buf: Buffer;
    if (!json.bits) {
      buf = Buffer.alloc(Math.ceil(json.m / 8));
    } else if (typeof json.bits === "string") {
      buf = Buffer.from(json.bits, "base64");
    } else if (Buffer.isBuffer(json.bits)) {
      buf = json.bits;
    } else if (
      json.bits &&
      typeof json.bits === "object" &&
      "buffer" in json.bits &&
      Buffer.isBuffer((json.bits as { buffer: unknown }).buffer)
    ) {
      buf = (json.bits as { buffer: Buffer }).buffer;
    } else if (
      json.bits &&
      typeof json.bits === "object" &&
      "value" in json.bits &&
      typeof (json.bits as { value: unknown }).value === "function"
    ) {
      buf = (json.bits as { value: () => Buffer }).value();
    } else {
      buf = Buffer.from(json.bits as ArrayLike<number> | string);
    }
    return new BloomFilter(json.m, json.k, buf);
  }
}
