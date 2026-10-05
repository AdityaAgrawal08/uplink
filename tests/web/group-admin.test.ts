import { describe, it, expect } from "vitest";
import { NextRequest } from "next/server";
import { redis } from "../../src/lib/redis";
import {
  createRoom,
  joinRoom,
  setRole,
  getRoomMeta,
  getRoster,
  roomExists,
  createInvite,
  getInvitesForUser,
  heartbeat,
} from "../../src/lib/rooms";
import { PATCH as metaPATCH } from "../../src/app/api/v1/session/[sessionId]/meta/route";
import { DELETE as sessionDELETE } from "../../src/app/api/v1/session/[sessionId]/route";
import { generateTestIdentity, signedNextRequest, type TestIdentity } from "./helpers/reqsig";

const SIG_ERROR = "Invalid or missing request signature";
const MEMBER_EDIT_403 = "Only admins can change group details";
const DELETE_403 = "Only the group creator can delete this group";

function rand(prefix: string): string {
  return `${prefix}_${Math.random().toString(36).slice(2, 9)}`; // stays within 3-20 alphanumeric/underscore
}

function params(sessionId: string) {
  return { params: Promise.resolve({ sessionId }) };
}

// A group owned by `creator`, with `member` joined and `admin` joined then
// promoted — all under their own real device keys (roster anchors).
async function makeGroup(opts: { maxMembers?: number; passwordHash?: string | null } = {}) {
  const creator = generateTestIdentity();
  const creatorName = rand("c");
  const { sessionId } = await createRoom(creatorName, creator.pubKeyB64, opts.passwordHash ?? null, {
    groupName: "Old Name",
    groupDesc: "old desc",
    maxMembers: opts.maxMembers,
    parentCode: "123456",
  });
  const member = generateTestIdentity();
  const memberName = rand("m");
  await joinRoom(sessionId, memberName, member.pubKeyB64);
  const admin = generateTestIdentity();
  const adminName = rand("a");
  await joinRoom(sessionId, adminName, admin.pubKeyB64);
  await setRole(sessionId, creatorName, adminName, "admin");
  return { sessionId, creator, creatorName, member, memberName, admin, adminName };
}

describe("PATCH /api/v1/session/[sessionId]/meta", () => {
  it("is 401 without a valid rostered signature", async () => {
    const { sessionId, creatorName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}/meta`;

    const unsigned = new NextRequest(url, {
      method: "PATCH",
      headers: { "Content-Type": "application/json", "X-Uplink-Username": creatorName },
      body: JSON.stringify({ groupName: "Hijacked" }),
    });
    const unsignedRes = await metaPATCH(unsigned, params(sessionId));
    expect(unsignedRes.status).toBe(401);
    expect((await unsignedRes.json()).error).toBe(SIG_ERROR);

    const stranger = generateTestIdentity();
    const forged = signedNextRequest(stranger, "PATCH", url, creatorName, { groupName: "Hijacked" });
    const forgedRes = await metaPATCH(forged as NextRequest, params(sessionId));
    expect(forgedRes.status).toBe(401);
    expect((await forgedRes.json()).error).toBe(SIG_ERROR);

    expect((await getRoomMeta(sessionId))?.groupName).toBe("Old Name");
  });

  it("refuses a plain member with the exact 403", async () => {
    const { sessionId, member, memberName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}/meta`;
    const req = signedNextRequest(member, "PATCH", url, memberName, { groupName: "Member Edit" });
    const res = await metaPATCH(req as NextRequest, params(sessionId));
    expect(res.status).toBe(403);
    expect((await res.json()).error).toBe(MEMBER_EDIT_403);
    expect((await getRoomMeta(sessionId))?.groupName).toBe("Old Name");
  });

  it("lets an admin rename + edit the description, persisting both and preserving every other meta field", async () => {
    const { sessionId, creatorName, admin, adminName } = await makeGroup({
      maxMembers: 8,
      passwordHash: "pbkdf2-test-hash",
    });
    const before = await getRoomMeta(sessionId);
    const url = `http://localhost/api/v1/session/${sessionId}/meta`;
    const req = signedNextRequest(admin, "PATCH", url, adminName, {
      groupName: "Renamed Squad",
      groupDesc: "fresh description",
    });
    const res = await metaPATCH(req as NextRequest, params(sessionId));
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ groupName: "Renamed Squad", groupDesc: "fresh description" });

    const after = await getRoomMeta(sessionId);
    expect(after).toMatchObject({
      groupName: "Renamed Squad",
      groupDesc: "fresh description",
      passwordHash: "pbkdf2-test-hash",
      creator: creatorName,
      maxMembers: 8,
      parentCode: "123456",
    });
    expect(after?.createdAt).toBe(before?.createdAt);
  });

  it("supports partial patches (creator included) without clobbering the other field", async () => {
    const { sessionId, creator, creatorName, admin, adminName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}/meta`;

    const descOnly = signedNextRequest(admin, "PATCH", url, adminName, { groupDesc: "" });
    const descRes = await metaPATCH(descOnly as NextRequest, params(sessionId));
    expect(descRes.status).toBe(200);
    expect(await descRes.json()).toEqual({ groupName: "Old Name", groupDesc: "" });

    const nameOnly = signedNextRequest(creator, "PATCH", url, creatorName, { groupName: "Creator Rename" });
    const nameRes = await metaPATCH(nameOnly as NextRequest, params(sessionId));
    expect(nameRes.status).toBe(200);
    expect(await nameRes.json()).toEqual({ groupName: "Creator Rename", groupDesc: "" });
    expect((await getRoomMeta(sessionId))?.groupName).toBe("Creator Rename");
    expect((await getRoomMeta(sessionId))?.groupDesc).toBe("");
  });

  it("validates lengths and types with createRoom's exact 400 messages", async () => {
    const { sessionId, admin, adminName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}/meta`;
    const bad: Array<{ body: Record<string, unknown>; error: string }> = [
      { body: { groupName: "" }, error: "groupName must be a string of 1-64 characters" },
      { body: { groupName: "x".repeat(65) }, error: "groupName must be a string of 1-64 characters" },
      { body: { groupName: 7 }, error: "groupName must be a string of 1-64 characters" },
      { body: { groupName: null }, error: "groupName must be a string of 1-64 characters" },
      { body: { groupDesc: "x".repeat(257) }, error: "groupDesc must be a string of at most 256 characters" },
      { body: { groupDesc: 42 }, error: "groupDesc must be a string of at most 256 characters" },
    ];
    for (const { body, error } of bad) {
      const req = signedNextRequest(admin, "PATCH", url, adminName, body);
      const res = await metaPATCH(req as NextRequest, params(sessionId));
      expect(res.status).toBe(400);
      expect((await res.json()).error).toBe(error);
    }
    // unchanged after all the 400s
    expect((await getRoomMeta(sessionId))?.groupName).toBe("Old Name");

    // boundary values are fine: 64-char name, 256-char description
    const boundary = signedNextRequest(admin, "PATCH", url, adminName, {
      groupName: "n".repeat(64),
      groupDesc: "d".repeat(256),
    });
    const boundaryRes = await metaPATCH(boundary as NextRequest, params(sessionId));
    expect(boundaryRes.status).toBe(200);
    expect(await boundaryRes.json()).toEqual({ groupName: "n".repeat(64), groupDesc: "d".repeat(256) });
    expect((await getRoomMeta(sessionId))?.groupDesc).toHaveLength(256);
  });

  it("rejects a non-object JSON body with 400", async () => {
    const { sessionId, admin, adminName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}/meta`;
    const req = signedNextRequest(admin, "PATCH", url, adminName, [] as unknown as Record<string, unknown>);
    const res = await metaPATCH(req as NextRequest, params(sessionId));
    expect(res.status).toBe(400);
    expect((await res.json()).error).toBe("Request body must be a JSON object");
  });
});

describe("DELETE /api/v1/session/[sessionId]", () => {
  it("is 401 without a valid rostered signature", async () => {
    const { sessionId, creatorName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}`;

    const unsigned = new NextRequest(url, {
      method: "DELETE",
      headers: { "X-Uplink-Username": creatorName },
    });
    const unsignedRes = await sessionDELETE(unsigned, params(sessionId));
    expect(unsignedRes.status).toBe(401);
    expect((await unsignedRes.json()).error).toBe(SIG_ERROR);

    const stranger = generateTestIdentity();
    const forged = signedNextRequest(stranger, "DELETE", url, creatorName);
    const forgedRes = await sessionDELETE(forged as NextRequest, params(sessionId));
    expect(forgedRes.status).toBe(401);
    expect((await forgedRes.json()).error).toBe(SIG_ERROR);
    expect(await roomExists(sessionId)).toBe(true);
  });

  it("refuses members and admins with the exact 403; the room survives", async () => {
    const { sessionId, member, memberName, admin, adminName } = await makeGroup();
    const url = `http://localhost/api/v1/session/${sessionId}`;
    const attempts: Array<[TestIdentity, string]> = [
      [member, memberName],
      [admin, adminName],
    ];
    for (const [id, name] of attempts) {
      const req = signedNextRequest(id, "DELETE", url, name);
      const res = await sessionDELETE(req as NextRequest, params(sessionId));
      expect(res.status).toBe(403);
      expect((await res.json()).error).toBe(DELETE_403);
    }
    expect(await roomExists(sessionId)).toBe(true);
    expect((await getRoster(sessionId)).length).toBe(3);
  });

  it("creator delete purges roster, invites, seat counter, per-user index and meta", async () => {
    const { sessionId, creator, creatorName, memberName } = await makeGroup({ maxMembers: 8 });
    const inviteeName = rand("i");
    await createInvite(sessionId, creatorName, inviteeName);
    // Everything the delete must reap exists up front.
    expect(String(await redis.get(`room:${sessionId}:seats`))).toBe("3"); // creator + member + admin
    expect(await redis.hlen(`room:${sessionId}:invites`)).toBe(1);
    expect(await redis.hgetall(`user:${inviteeName}:invites`)).toMatchObject({ [sessionId]: "1" });

    const url = `http://localhost/api/v1/session/${sessionId}`;
    const req = signedNextRequest(creator, "DELETE", url, creatorName);
    const res = await sessionDELETE(req as NextRequest, params(sessionId));
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });

    expect(await roomExists(sessionId)).toBe(false);
    expect(await getRoomMeta(sessionId)).toBeNull();
    expect(await getRoster(sessionId)).toEqual([]);
    expect(await redis.hgetall(`room:${sessionId}:members`)).toBeNull();
    expect(await redis.hgetall(`room:${sessionId}:invites`)).toBeNull();
    expect(await redis.get(`room:${sessionId}:seats`)).toBeNull();
    expect(await redis.hgetall(`user:${inviteeName}:invites`)).toBeNull();
    expect(await getInvitesForUser(inviteeName)).toEqual([]);
    expect(await redis.lrange("room:index", 0, -1)).not.toContain(sessionId);

    // Survivors meet a vanished room on their next beat (client drops it).
    await expect(heartbeat(sessionId, memberName, {})).rejects.toMatchObject({
      status: 404,
      message: "Session not found",
    });
  });

  it("non-members cannot delete (no roster anchor: 401)", async () => {
    const { sessionId } = await makeGroup();
    const outsider = generateTestIdentity();
    const outsiderName = rand("o");
    const url = `http://localhost/api/v1/session/${sessionId}`;
    const req = signedNextRequest(outsider, "DELETE", url, outsiderName);
    const res = await sessionDELETE(req as NextRequest, params(sessionId));
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe(SIG_ERROR);
    expect(await roomExists(sessionId)).toBe(true);
  });
});
