import crypto from "crypto";
import { NextRequest, NextResponse } from "next/server";
import { apiError } from "@/lib/api-utils";
import { redis } from "@/lib/redis";

// ─── Request signatures (identity-bound signaling auth) ────────────────────
//
// Every identity-bearing signaling call proves possession of the device
// identity key — the X25519 static keypair whose public half was claimed
// immutably in the room roster at join (hsetnx). A raw X-Uplink-Username
// header alone authorizes nothing: the caller must sign
//
//   METHOD "|" PATH "|" TIMESTAMP_MS "|" NONCE
//
// with that key, and we verify against the pubkey we already hold for the
// claimed username (the roster entry / the invite-accept body / the
// per-user invite claim). Impersonation therefore requires forging an
// Ed25519 signature under the victim's key.
//
// X25519 keys cannot sign directly, so the CLI signs with the Edwards-form
// of the SAME private scalar (a = k mod L). Because the RFC 7748 §4.1
// birational map (u = (1+y)/(1−y), v = sqrt(−486664)·u/x) is a group
// isomorphism sending the Edwards base point to u = 9, the point
// A = a·B has u-coordinate exactly equal to the X25519 public key. We
// recover A from the rostered pubkey (y = (u−1)/(u+1), x with sign bit 0)
// and run a stock Ed25519 verify — no key conversion on the client, no
// extra keys, no new environment.
//
// Replay protection: a 30s timestamp window plus a per-username one-time
// (timestamp, nonce) consumption in Redis (SET NX, short TTL).

// BigInt literals are ES2020; this build targets ES2017, so constants are
// built through BigInt(n) instead.
const b = (n: number | string): bigint => BigInt(n);

export const REQUEST_SIGNATURE_WINDOW_MS = 30_000;
export const REQUEST_SIGNATURE_NONCE_TTL_SEC = 120;
export const REQUEST_SIGNATURE_ERROR = "Invalid or missing request signature";

export function requestSignatureMessage(method: string, path: string, timestamp: string, nonce: string): string {
  return `${method}|${path}|${timestamp}|${nonce}`;
}

// ─── X25519 pubkey → Ed25519 pubkey (RFC 7748 §4.1 birational map) ─────────

const P = (b(1) << b(255)) - b(19);
// d = -121665 / 121666 mod p
const D = mod(b(-121665) * modInv(b(121666), P), P);
// sqrt(-1) = 2^((p-1)/4)
const SQRT_M1 = modPow(b(2), (P - b(1)) >> b(2), P);

function mod(a: bigint, m: bigint): bigint {
  const r = a % m;
  return r < b(0) ? r + m : r;
}

function modPow(base: bigint, exp: bigint, m: bigint): bigint {
  base = mod(base, m);
  let result = b(1);
  while (exp > b(0)) {
    if (exp & b(1)) result = (result * base) % m;
    base = (base * base) % m;
    exp >>= b(1);
  }
  return result;
}

function modInv(a: bigint, m: bigint): bigint {
  return modPow(mod(a, m), m - b(2), m);
}

function leToBig(buf: Buffer): bigint {
  const hex = [...buf].reverse().map((b) => b.toString(16).padStart(2, "0")).join("");
  return BigInt("0x" + hex);
}

function bigToLe(x: bigint, len: number): Buffer {
  const buf = Buffer.alloc(len);
  let v = x;
  for (let i = 0; i < len; i++) {
    buf[i] = Number(v & b(0xff));
    v >>= b(8);
  }
  return buf;
}

// convertX25519PubkeyToEd25519 derives the compressed Ed25519 public key
// (sign bit 0) that a device key signs with, from its base64 X25519
// Montgomery u-coordinate. Returns null for any malformed input.
export function convertX25519PubkeyToEd25519(pubkeyB64: string): Buffer | null {
  let raw: Buffer;
  try {
    raw = Buffer.from(pubkeyB64, "base64");
  } catch {
    return null;
  }
  if (raw.length !== 32) return null;
  const u = leToBig(raw);
  if (u >= P) return null;
  // y = (u - 1) / (u + 1) mod p
  const uPlus1 = mod(u + b(1), P);
  if (uPlus1 === b(0)) return null; // the point at infinity on the Edwards side
  const y = mod((u - b(1)) * modInv(uPlus1, P), P);
  // x² = (y² - 1) / (d·y² + 1) mod p
  const y2 = mod(y * y, P);
  const den = mod(D * y2 + b(1), P);
  if (den === b(0)) return null;
  const x2 = mod((y2 - b(1)) * modInv(den, P), P);
  // sqrt mod p (p ≡ 5 mod 8): x = x2^((p+3)/8); if that isn't a root,
  // multiply by sqrt(-1).
  let x = modPow(x2, (P + b(3)) >> b(3), P);
  if (mod(x * x, P) !== x2) x = mod(x * SQRT_M1, P);
  if (mod(x * x, P) !== x2) return null; // u not on the Edwards curve
  if (x & b(1)) x = P - x; // force sign bit 0 — matches what the client signs with
  const out = bigToLe(y, 32);
  return out;
}

// verifyRequestSignature is the full gate: header presence, 30s timestamp
// window, Ed25519 verification against the anchor pubkey, then an atomic
// single-use (timestamp, nonce) consumption. `pubkey` is the anchor the
// route resolved for the CLAIMED username (roster entry, accept body, or
// per-user invite claim); null anchors always fail.
export async function verifyRequestSignature(opts: {
  username: string;
  method: string;
  path: string;
  timestamp: string;
  nonce: string;
  signature: string;
  pubkey: string | null;
}): Promise<boolean> {
  const { username, method, path, timestamp, nonce, signature, pubkey } = opts;
  if (!pubkey) return false;
  if (typeof timestamp !== "string" || timestamp.length === 0 || timestamp.length > 20) return false;
  const ts = Number(timestamp);
  if (!Number.isFinite(ts) || ts <= 0) return false;
  if (Math.abs(Date.now() - ts) > REQUEST_SIGNATURE_WINDOW_MS) return false;
  if (typeof nonce !== "string" || nonce.length < 8 || nonce.length > 128) return false;
  const sig = Buffer.from(signature, "base64");
  if (sig.length !== 64) return false;
  const pub = convertX25519PubkeyToEd25519(pubkey);
  if (!pub) return false;
  const der = crypto.createPublicKey({
    key: Buffer.concat([ED25519_SPKI_PREFIX, pub]),
    format: "der",
    type: "spki",
  });
  try {
    const ok = crypto.verify(
      null,
      Buffer.from(requestSignatureMessage(method, path, timestamp, nonce), "utf8"),
      der,
      sig
    );
    if (!ok) return false;
  } catch {
    return false;
  }
  // Replay guard: consume the (timestamp, nonce) pair atomically. The
  // digest namespaced under the username keeps key sizes bounded.
  const digest = crypto.createHash("sha256").update(`${timestamp}|${nonce}`).digest("hex");
  const claimed = await redis.set(`sig:nonce:${username}:${digest}`, "1", {
    ex: REQUEST_SIGNATURE_NONCE_TTL_SEC,
    nx: true,
  });
  return claimed !== null;
}

// ED25519_SPKI_PREFIX is the DER subjectPublicKeyInfo header for Ed25519
// (OID 1.3.101.112 + BIT STRING), so a raw 32-byte key verifies directly.
const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

// requireRequestSignature gates one identity-bearing signaling call with the
// standard 401 answer. `anchor` is the pubkey resolved for the claimed
// username; passing null (no anchor) rejects every request.
export async function requireRequestSignature(
  req: NextRequest,
  username: string,
  anchor: string | null
): Promise<true | NextResponse> {
  const ok = await verifyRequestSignature({
    username,
    method: req.method,
    path: req.nextUrl.pathname,
    timestamp: req.headers.get("X-Uplink-Timestamp") ?? "",
    nonce: req.headers.get("X-Uplink-Nonce") ?? "",
    signature: req.headers.get("X-Uplink-Sig") ?? "",
    pubkey: anchor,
  });
  return ok ? true : apiError(REQUEST_SIGNATURE_ERROR, 401);
}