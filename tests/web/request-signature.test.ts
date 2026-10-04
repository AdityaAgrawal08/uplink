import { describe, it, expect } from "vitest";
import crypto from "crypto";
import {
  convertX25519PubkeyToEd25519,
  verifyRequestSignature,
  requestSignatureMessage,
  REQUEST_SIGNATURE_ERROR,
} from "../../src/lib/request-signature";
import { generateTestIdentity, signRequest, requestSignatureMessage as testMsg } from "./helpers/reqsig";

// Cross-language vector, pinned from the Go signer (cli/reqsig_test.go
// TestReqSigFixedVector): the same pubkey + signature must verify here.
const GO_VECTOR = {
  pubkey: "pOCSkrZRwni5dyxWn1+puxPZBrRqtoyd+dwrRAn4ogk=",
  signature: "yHoToVLbjMlw7eMV8RvQerlKWtoPHBCn5YZgjppdphIR/7+NsPCdHUyW5GiIqZalHNNKUUxhfLG/4D+F/XMaCw==",
  message: "POST|/api/v1/session/123456/heartbeat|1720000000000|00112233445566778899aabbccddeeff",
};

describe("request signatures", () => {
  it("converts the X25519 base point (u=9) to the Ed25519 generator", () => {
    const gen = Buffer.from("5866666666666666666666666666666666666666666666666666666666666666", "hex");
    const u9 = Buffer.alloc(32);
    u9[0] = 9;
    expect(convertX25519PubkeyToEd25519(u9.toString("base64"))?.equals(gen)).toBe(true);
  });

  it("rejects malformed anchors", () => {
    expect(convertX25519PubkeyToEd25519("not-base64!")).toBeNull();
    expect(convertX25519PubkeyToEd25519(Buffer.alloc(16).toString("base64"))).toBeNull();
    // u = p-1 (the Edwards point at infinity): y = (u-1)/(u+1) has a zero
    // denominator.
    const pminus1 = Buffer.alloc(32);
    pminus1[0] = 0xec;
    pminus1[31] = 0x7f;
    expect(convertX25519PubkeyToEd25519(pminus1.toString("base64"))).toBeNull();
    // All-zero u: on the curve, but it is the point paying no key.
    expect(convertX25519PubkeyToEd25519(Buffer.alloc(32).toString("base64"))).not.toBeNull();
  });

  it("verifies the Go-generated fixed vector (cross-language)", () => {
    const pub = convertX25519PubkeyToEd25519(GO_VECTOR.pubkey);
    expect(pub).not.toBeNull();
    const der = crypto.createPublicKey({
      key: Buffer.concat([Buffer.from("302a300506032b6570032100", "hex"), pub!]),
      format: "der",
      type: "spki",
    });
    expect(crypto.verify(null, Buffer.from(GO_VECTOR.message, "utf8"), der, Buffer.from(GO_VECTOR.signature, "base64"))).toBe(true);
    // Tampered message must fail.
    expect(
      crypto.verify(null, Buffer.from(GO_VECTOR.message + "x", "utf8"), der, Buffer.from(GO_VECTOR.signature, "base64"))
    ).toBe(false);
  });

  it("signer and server derivation agree on the same key (montgomery consistency)", () => {
    for (let i = 0; i < 5; i++) {
      const id = generateTestIdentity();
      const { edKey } = signRequest(id, "POST", "/api/v1/session/123456/heartbeat", "1720000000000", "nonce1234");
      const converted = convertX25519PubkeyToEd25519(id.pubKeyB64);
      expect(converted?.equals(Buffer.from(edKey))).toBe(true);
      // Sign bit 0 always.
      expect(edKey[31] & 0x80).toBe(0);
    }
  });

  it("verifyRequestSignature: full gate passes for a fresh signed request", async () => {
    const id = generateTestIdentity();
    const path = "/api/v1/session/123456/heartbeat";
    const ts = String(Date.now());
    const nonce = crypto.randomBytes(16).toString("hex");
    const msg = requestSignatureMessage("POST", path, ts, nonce);
    const { signature } = signRequest(id, "POST", path, ts, nonce);
    expect(msg).toBe(testMsg("POST", path, ts, nonce));
    expect(
      await verifyRequestSignature({
        username: "alice",
        method: "POST",
        path,
        timestamp: ts,
        nonce,
        signature,
        pubkey: id.pubKeyB64,
      })
    ).toBe(true);
  });

  it("verifyRequestSignature: stale timestamp is rejected", async () => {
    const id = generateTestIdentity();
    const path = "/api/v1/session/123456/heartbeat";
    const ts = String(Date.now() - 60_000); // beyond the 30s window
    const nonce = crypto.randomBytes(16).toString("hex");
    const { signature } = signRequest(id, "POST", path, ts, nonce);
    expect(
      await verifyRequestSignature({
        username: "alice",
        method: "POST",
        path,
        timestamp: ts,
        nonce,
        signature,
        pubkey: id.pubKeyB64,
      })
    ).toBe(false);
  });

  it("verifyRequestSignature: future timestamp is rejected", async () => {
    const id = generateTestIdentity();
    const ts = String(Date.now() + 60_000);
    const nonce = crypto.randomBytes(16).toString("hex");
    const { signature } = signRequest(id, "POST", "/x", ts, nonce);
    expect(
      await verifyRequestSignature({
        username: "alice",
        method: "POST",
        path: "/x",
        timestamp: ts,
        nonce,
        signature,
        pubkey: id.pubKeyB64,
      })
    ).toBe(false);
  });

  it("verifyRequestSignature: replay (same timestamp+nonce) is rejected", async () => {
    const id = generateTestIdentity();
    const path = "/api/v1/session/123456/heartbeat";
    const ts = String(Date.now());
    const nonce = "aabbccddeeff00112233445566778899";
    const { signature } = signRequest(id, "POST", path, ts, nonce);
    const opts = { username: "alice", method: "POST", path, timestamp: ts, nonce, signature, pubkey: id.pubKeyB64 };
    expect(await verifyRequestSignature(opts)).toBe(true);
    expect(await verifyRequestSignature(opts)).toBe(false);
    // A DIFFERENT username may reuse the nonce (nonce keys are per-user) —
    // but only with a valid signature for THAT user's key.
    const other = generateTestIdentity();
    const sig2 = signRequest(other, "POST", path, ts, nonce).signature;
    expect(
      await verifyRequestSignature({ ...opts, username: "bob", signature: sig2, pubkey: other.pubKeyB64 })
    ).toBe(true);
  });

  it("verifyRequestSignature: wrong key / missing pieces are rejected", async () => {
    const id = generateTestIdentity();
    const other = generateTestIdentity();
    const ts = String(Date.now());
    const nonce = crypto.randomBytes(16).toString("hex");
    const { signature } = signRequest(id, "POST", "/x", ts, nonce);
    const base = { username: "alice", method: "POST", path: "/x", timestamp: ts, nonce, signature, pubkey: id.pubKeyB64 };
    // Signature made under a DIFFERENT key (the impersonation case).
    expect(await verifyRequestSignature({ ...base, pubkey: other.pubKeyB64 })).toBe(false);
    // Missing signature / nonce / timestamp.
    expect(await verifyRequestSignature({ ...base, signature: "" })).toBe(false);
    expect(await verifyRequestSignature({ ...base, nonce: "" })).toBe(false);
    expect(await verifyRequestSignature({ ...base, timestamp: "" })).toBe(false);
    // Garbage signature.
    expect(await verifyRequestSignature({ ...base, signature: "AAAA" })).toBe(false);
    // No anchor at all.
    expect(await verifyRequestSignature({ ...base, pubkey: null })).toBe(false);
    expect(REQUEST_SIGNATURE_ERROR).toBe("Invalid or missing request signature");
  });
});