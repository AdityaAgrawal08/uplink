import { describe, it, expect } from "vitest";
import { BloomFilter } from "../bloom";

const mk = (m = 1024, k = 4) => new BloomFilter(m, k);

describe("BloomFilter", () => {
  it("added items are always contained (no false negatives)", () => {
    const bf = mk();
    const names = ["alice", "bob", "carol_99", "UPPER_case1"];
    for (const n of names) bf.add(n);
    for (const n of names) expect(bf.contains(n)).toBe(true);
  });

  it("un-added items are usually rejected", () => {
    const bf = mk(8192, 6); // large enough to keep FP rate tiny
    for (let i = 0; i < 100; i++) bf.add(`user${i}`);
    let fp = 0;
    for (let i = 100; i < 300; i++) if (bf.contains(`user${i}`)) fp++;
    expect(fp).toBeLessThanOrEqual(2); // ~0 expected at this sizing
    expect(bf.contains("never_added_xyz")).toBe(false);
  });

  it("JSON round-trip preserves membership", () => {
    const bf = mk();
    bf.add("roundtrip");
    const restored = BloomFilter.fromJSON(JSON.parse(JSON.stringify(bf.toJSON())));
    expect(restored.contains("roundtrip")).toBe(true);
    expect(restored.contains("absent")).toBe(false);
  });

  it("fromJSON tolerates null/undefined bits", () => {
    const fresh = BloomFilter.fromJSON({ m: 64, k: 3, bits: undefined });
    expect(fresh.contains("anything")).toBe(false);
  });

  it("empty filter contains nothing", () => {
    expect(mk().contains("alice")).toBe(false);
  });
});
