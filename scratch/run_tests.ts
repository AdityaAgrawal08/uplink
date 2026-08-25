import test from "node:test";
import assert from "node:assert";
import { sanitizeFilename, anonymizeIp, hashPassword, verifyPassword } from "../src/lib/crypto";
import { crc64nvme, crc64nvmeBase64 } from "../src/lib/crc64";

test("Sanitize Filename traversal and chars", () => {
  assert.strictEqual(sanitizeFilename("../../etc/passwd"), "passwd");
  assert.strictEqual(sanitizeFilename("hello world! @123.txt"), "hello_world___123.txt");
  // Unified fallback token — must match the Go CLI (cli/main.go) exactly.
  assert.strictEqual(sanitizeFilename(""), "file");
  assert.strictEqual(sanitizeFilename("."), "file");
  assert.strictEqual(sanitizeFilename(".."), "file");
});

test("IP Anonymization dev mode fallback key and hashing consistency", () => {
  const ip = "192.168.1.1";
  const hash1 = anonymizeIp(ip);
  const hash2 = anonymizeIp(ip);
  assert.strictEqual(hash1, hash2);
  assert.match(hash1, /^[a-f0-9]{64}$/);
});

test("Password hashing and verification with Argon2id", async () => {
  const pw = "superSecret123!";
  const hash = await hashPassword(pw);
  assert.ok(hash.startsWith("$argon2id$"));
  
  const isValid = await verifyPassword(pw, hash);
  assert.strictEqual(isValid, true);

  const isInvalid = await verifyPassword("wrongPassword", hash);
  assert.strictEqual(isInvalid, false);
});

test("CRC64NVMe computations", () => {
  const data = new TextEncoder().encode("123456789");
  const crc = crc64nvme(data);
  const b64 = crc64nvmeBase64(data);
  assert.strictEqual(typeof crc, "bigint");
  assert.strictEqual(b64, "eADAZNSoN4Q=");
});

test("MockRedis does not auto-parse strings resembling JSON", async () => {
  const { redis } = await import("../src/lib/redis");
  await redis.set("test:num_str", "12345");
  const val = await redis.get("test:num_str");
  assert.strictEqual(typeof val, "string");
  assert.strictEqual(val, "12345");

  await redis.set("test:obj", { a: 1 });
  const valObj = await redis.get("test:obj");
  assert.strictEqual(typeof valObj, "object");
  assert.deepStrictEqual(valObj, { a: 1 });
});
