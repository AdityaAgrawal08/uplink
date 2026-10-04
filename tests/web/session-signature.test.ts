import { describe, it, expect } from "vitest";
import { NextRequest } from "next/server";
import { createRoom, joinRoom, createInvite, getRoster, getRoomMeta } from "../../src/lib/rooms";
import { POST as kickPOST } from "../../src/app/api/v1/session/[sessionId]/kick/route";
import { POST as adminPOST } from "../../src/app/api/v1/session/[sessionId]/admin/route";
import { POST as leavePOST } from "../../src/app/api/v1/session/[sessionId]/leave/route";
import { POST as ackPOST } from "../../src/app/api/v1/session/[sessionId]/inbox/ack/route";
import { POST as acceptPOST } from "../../src/app/api/v1/session/[sessionId]/invites/accept/route";
import { generateTestIdentity, signRequest, signedNextRequest } from "./helpers/reqsig";
import crypto from "crypto";

const SIG_ERROR = "Invalid or missing request signature";

function rand(prefix: string): string {
  return `${prefix}_${Math.random().toString(36).slice(2, 9)}`;
}

// A room owned by `creator` (real device key) with `member` joined under
// their own real key.
async function makeRoomWithNames() {
  const creator = generateTestIdentity();
  const creatorName = rand("c");
  const { sessionId } = await createRoom(creatorName, creator.pubKeyB64, null);
  const member = generateTestIdentity();
  const memberName = rand("m");
  await joinRoom(sessionId, memberName, member.pubKeyB64);
  return { sessionId, creator, creatorName, member, memberName };
}

async function makeInviteFor() {
  const host = generateTestIdentity();
  const hostName = rand("h");
  const { sessionId } = await createRoom(hostName, host.pubKeyB64, null);
  const invitee = generateTestIdentity();
  const inviteeName = rand("i");
  await createInvite(sessionId, hostName, inviteeName);
  return { sessionId, invitee, inviteeName };
}

describe("session routes require a valid request signature", () => {
  it("kick: spoofed username (no signature) is 401 with the distinct message", async () => {
    const { sessionId, memberName } = await makeRoomWithNames();
    const req = new NextRequest(`http://localhost/api/v1/session/${sessionId}/kick`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Uplink-Username": rand("u") },
      body: JSON.stringify({ target: memberName }),
    });
    const res = await kickPOST(req, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
  });

  it("kick: a valid signature passes the gate (authorization still applies)", async () => {
    const { sessionId, memberName, creator, creatorName } = await makeRoomWithNames();
    const path = `/api/v1/session/${sessionId}/kick`;
    const req = signedNextRequest(creator, "POST", `http://localhost${path}`, creatorName, {
      target: memberName,
    });
    const res = await kickPOST(req as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(200);
    expect((await getRoster(sessionId)).map((m) => m.username)).not.toContain(memberName);
  });

  it("kick: signature under a non-rostered key is 401", async () => {
    const { sessionId, memberName, creatorName } = await makeRoomWithNames();
    const stranger = generateTestIdentity();
    const path = `/api/v1/session/${sessionId}/kick`;
    const req = signedNextRequest(stranger, "POST", `http://localhost${path}`, creatorName, {
      target: memberName,
    });
    const res = await kickPOST(req as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
  });

  it("admin: spoofed username is 401; a valid creator signature passes", async () => {
    const { sessionId, memberName, creator, creatorName } = await makeRoomWithNames();
    const spoof = new NextRequest(`http://localhost/api/v1/session/${sessionId}/admin`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Uplink-Username": creatorName },
      body: JSON.stringify({ target: memberName, admin: true }),
    });
    const spoofRes = await adminPOST(spoof, { params: Promise.resolve({ sessionId }) });
    expect(spoofRes.status).toBe(401);
    expect((await spoofRes.json()).error).toBe(SIG_ERROR);

    const path = `/api/v1/session/${sessionId}/admin`;
    const ok = signedNextRequest(creator, "POST", `http://localhost${path}`, creatorName, {
      target: memberName,
      admin: true,
    });
    const okRes = await adminPOST(ok as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(okRes.status).toBe(200);
  });

  it("leave: unsigned leave is 401 (crown transfer requires the signed leave)", async () => {
    const { sessionId, creatorName } = await makeRoomWithNames();
    const req = new NextRequest(`http://localhost/api/v1/session/${sessionId}/leave`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Uplink-Username": creatorName },
      body: "{}",
    });
    const res = await leavePOST(req, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
  });

  it("leave: a signed leave succeeds and transfers the crown", async () => {
    const { sessionId, creator, creatorName, memberName } = await makeRoomWithNames();
    const path = `/api/v1/session/${sessionId}/leave`;
    const req = signedNextRequest(creator, "POST", `http://localhost${path}`, creatorName);
    const res = await leavePOST(req as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(200);
    const meta = await getRoomMeta(sessionId);
    expect(meta?.creator).toBe(memberName); // crown moved to the survivor
    expect((await getRoster(sessionId)).find((m) => m.role === "creator")?.username).toBe(memberName);
  });

  it("inbox/ack: spoofed username is 401 (inbox destruction vector)", async () => {
    const { sessionId, memberName } = await makeRoomWithNames();
    const req = new NextRequest(`http://localhost/api/v1/session/${sessionId}/inbox/ack`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Uplink-Username": memberName },
      body: JSON.stringify({ ids: ["m1"] }),
    });
    const res = await ackPOST(req, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
  });

  it("accept: no signature / wrong key are 401; the true key binds the seat", async () => {
    const { sessionId, invitee, inviteeName } = await makeInviteFor();
    const path = `/api/v1/session/${sessionId}/invites/accept`;
    const payload = { code: sessionId, pubkey: invitee.pubKeyB64 };

    // No signature at all.
    const plain = new NextRequest(`http://localhost${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Uplink-Username": inviteeName },
      body: JSON.stringify(payload),
    });
    const plainRes = await acceptPOST(plain, { params: Promise.resolve({ sessionId }) });
    expect(plainRes.status).toBe(401);
    expect((await plainRes.json()).error).toBe(SIG_ERROR);

    // A cryptographically VALID signature under a DIFFERENT key than the
    // body pubkey: still 401 (the seat is bound to the signing key).
    const impostor = generateTestIdentity();
    const wrong = signedNextRequest(impostor, "POST", `http://localhost${path}`, inviteeName, payload);
    const wrongRes = await acceptPOST(wrong as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(wrongRes.status).toBe(401);

    // True key + matching body pubkey: joins and consumes the invite.
    const ok = signedNextRequest(invitee, "POST", `http://localhost${path}`, inviteeName, payload);
    const okRes = await acceptPOST(ok as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(okRes.status).toBe(200);
    expect((await getRoster(sessionId)).map((m) => m.username)).toContain(inviteeName);
  });

  it("replay: the same signed request twice is 401 the second time", async () => {
    const { sessionId, creator, creatorName, memberName } = await makeRoomWithNames();
    const path = `/api/v1/session/${sessionId}/kick`;
    const ts = String(Date.now());
    const nonce = crypto.randomBytes(16).toString("hex");
    const { signature } = signRequest(creator, "POST", path, ts, nonce);
    const build = () =>
      new NextRequest(`http://localhost${path}`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Uplink-Username": creatorName,
          "X-Uplink-Timestamp": ts,
          "X-Uplink-Nonce": nonce,
          "X-Uplink-Sig": signature,
        },
        body: JSON.stringify({ target: memberName }),
      });
    const first = await kickPOST(build(), { params: Promise.resolve({ sessionId }) });
    expect(first.status).toBe(200);
    const second = await kickPOST(build(), { params: Promise.resolve({ sessionId }) });
    expect(second.status).toBe(401);
    expect((await second.json()).error).toBe(SIG_ERROR);
  });

  it("stale: a signed request older than the 30s window is 401", async () => {
    const { sessionId, creator, creatorName, memberName } = await makeRoomWithNames();
    const path = `/api/v1/session/${sessionId}/kick`;
    const req = signedNextRequest(creator, "POST", `http://localhost${path}`, creatorName, { target: memberName }, {
      timestamp: String(Date.now() - 60_000),
    });
    const res = await kickPOST(req as NextRequest, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
  });

  it("tampered username: a valid signature under a different name is 401", async () => {
    const { sessionId, creator, memberName } = await makeRoomWithNames();
    const path = `/api/v1/session/${sessionId}/kick`;
    // Signed AS the creator, but the request is labeled with another
    // member's name: the anchor lookup follows the CLAIMED name, so the
    // creator's key cannot vouch for it.
    const ts = String(Date.now());
    const nonce = crypto.randomBytes(16).toString("hex");
    const { signature } = signRequest(creator, "POST", path, ts, nonce);
    const req = new NextRequest(`http://localhost${path}`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Uplink-Username": memberName,
        "X-Uplink-Timestamp": ts,
        "X-Uplink-Nonce": nonce,
        "X-Uplink-Sig": signature,
      },
      body: JSON.stringify({ target: memberName }),
    });
    const res = await kickPOST(req, { params: Promise.resolve({ sessionId }) });
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
  });
});