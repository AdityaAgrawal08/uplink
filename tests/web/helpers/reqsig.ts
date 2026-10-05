// Test-side request signer: mirrors the Go CLI signer (cli/reqsig.go)
// exactly, so cross-language vectors and route tests both hold. The device
// identity is an X25519 keypair (Node crypto); signatures are Ed25519 over
// METHOD|PATH|TIMESTAMP|NONCE using the X25519 scalar mod L as the Edwards
// scalar (the RFC 7748 birational map makes mont(A) == the public key, so
// the server-side conversion and this signer always agree).
import crypto from "crypto";
import { ed25519 } from "@noble/curves/ed25519.js";

const L = BigInt("0x1000000000000000000000000000000014def9dea2f79cd65812631a5cf5d3ed");
const b = (n: number | string): bigint => BigInt(n);

export interface TestIdentity {
  rawPriv: Buffer; // the raw X25519 scalar as GenerateKey produced it
  pubKeyB64: string;
}

// generateTestIdentity mints a device identity (raw scalar + base64 pubkey).
export function generateTestIdentity(): TestIdentity {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("x25519");
  const pubJwk = publicKey.export({ format: "jwk" }) as { x: string };
  const privJwk = privateKey.export({ format: "jwk" }) as { d: string };
  return {
    rawPriv: Buffer.from(privJwk.d, "base64url"),
    pubKeyB64: Buffer.from(pubJwk.x, "base64url").toString("base64"),
  };
}

function clamp(b: Buffer): Buffer {
  const c = Buffer.from(b);
  c[0] &= 248;
  c[31] &= 127;
  c[31] |= 64;
  return c;
}

function leToBig(buf: Buffer): bigint {
  const hex = [...buf].reverse().map((x) => x.toString(16).padStart(2, "0")).join("");
  return BigInt("0x" + hex);
}

function bigToLe(x: bigint, len: number): Buffer {
  const out = Buffer.alloc(len);
  let v = x;
  for (let i = 0; i < len; i++) {
    out[i] = Number(v & b(0xff));
    v >>= b(8);
  }
  return out;
}

export function requestSignatureMessage(method: string, path: string, timestamp: string, nonce: string): string {
  return `${method}|${path}|${timestamp}|${nonce}`;
}

// signRequest signs one request payload with a test identity. The returned
// A is forced to sign bit 0 (the server derives A from the pubkey with sign
// bit 0); the exposed edKey lets tests assert signer/verifier agreement.
export function signRequest(
  id: TestIdentity,
  method: string,
  path: string,
  timestamp: string,
  nonce: string,
): { signature: string; edKey: Uint8Array } {
  const k = clamp(id.rawPriv);
  let s = leToBig(k) % L;
  let A = ed25519.Point.BASE.multiply(s);
  if (A.toBytes()[31] & 0x80) {
    s = L - s;
    A = A.negate();
  }
  const msg = Buffer.from(requestSignatureMessage(method, path, timestamp, nonce), "utf8");
  const h = crypto.createHash("sha512").update(k).digest();
  const r = leToBig(crypto.createHash("sha512").update(Buffer.concat([h.subarray(32), msg])).digest()) % L;
  const R = ed25519.Point.BASE.multiply(r);
  const hram =
    leToBig(
      crypto.createHash("sha512").update(Buffer.concat([Buffer.from(R.toBytes()), Buffer.from(A.toBytes()), msg])).digest()
    ) % L;
  const S = (r + hram * s) % L;
  const sig = Buffer.concat([Buffer.from(R.toBytes()), bigToLe(S, 32)]);
  return { signature: sig.toString("base64"), edKey: A.toBytes() };
}

// signedNextRequest builds a NextRequest carrying a valid signature for the
// given identity/method/path (username header filled from `username`).
export function signedNextRequest(
  id: TestIdentity,
  method: string,
  url: string,
  username: string,
  body?: Record<string, unknown>,
  opts: { timestamp?: string; nonce?: string; pubkeyHeader?: boolean } = {}
): Request {
  const path = new URL(url).pathname;
  const timestamp = opts.timestamp ?? String(Date.now());
  const nonce = opts.nonce ?? crypto.randomBytes(16).toString("hex");
  const { signature } = signRequest(id, method, path, timestamp, nonce);
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Uplink-Username": username,
    "X-Uplink-Timestamp": timestamp,
    "X-Uplink-Nonce": nonce,
    "X-Uplink-Sig": signature,
  };
  if (opts.pubkeyHeader) headers["X-Uplink-Pubkey"] = id.pubKeyB64;
  const init: RequestInit = { method, headers };
  if (body !== undefined) init.body = JSON.stringify(body);
  // Narrow to NextRequest's RequestInit (its AbortSignal is non-nullable).
  const reqInit: NextRequestInit = init as NextRequestInit;
  return new NextRequestLike(url, reqInit);
}

// Grab the real NextRequest class without importing next/server in helpers
// (the test files import it directly when they need the concrete type).
import { NextRequest } from "next/server";
type NextRequestInit = ConstructorParameters<typeof NextRequest>[1];
const NextRequestLike = NextRequest;