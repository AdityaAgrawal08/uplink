import { describe, it, expect } from "vitest";
import { NextRequest } from "next/server";
import { redis } from "../../src/lib/redis";
import {
  RoomError,
  createRoom,
  joinRoom,
  getRoomMeta,
  getRoster,
  leaveRoom,
  kickMember,
  setRole,
  roomExists,
  createInvite,
  getInvitesForUser,
  acceptInvite,
  declineInvite,
  MAX_PENDING_INVITES,
} from "../../src/lib/rooms";
import { hashPassword } from "../../src/lib/crypto";
import { POST as acceptInvitePOST } from "../../src/app/api/v1/session/[sessionId]/invites/accept/route";

const PUBKEY = Buffer.alloc(32, 7).toString("base64");

function rand(prefix: string): string {
  return `${prefix}_${Math.random().toString(36).slice(2, 9)}`; // stays within 3-20 alphanumeric/underscore
}

async function makeRoom(
  opts: { groupName?: string; groupDesc?: string; maxMembers?: number; parentCode?: string; passwordHash?: string | null } = {}
) {
  const { passwordHash, ...roomOpts } = opts;
  const username = rand("u");
  const { sessionId } = await createRoom(username, PUBKEY, passwordHash ?? null, roomOpts);
  return { sessionId, username };
}

describe("group rooms: meta, capacity, crown", () => {
  it("create accepts group meta and exposes it via getRoomMeta", async () => {
    const { sessionId } = await makeRoom({
      groupName: "Friday Squad",
      groupDesc: "weekend plans",
      maxMembers: 8,
      parentCode: "123456",
    });
    const meta = await getRoomMeta(sessionId);
    expect(meta).toMatchObject({
      groupName: "Friday Squad",
      groupDesc: "weekend plans",
      maxMembers: 8,
      parentCode: "123456",
      creator: expect.any(String),
    });
  });

  it("plain rooms default to null group meta (unlimited capacity)", async () => {
    const { sessionId } = await makeRoom();
    const meta = await getRoomMeta(sessionId);
    expect(meta).toMatchObject({ groupName: null, groupDesc: null, maxMembers: null, parentCode: null });
  });

  it("create validates group meta lengths and maxMembers >= 2", async () => {
    await expect(createRoom(rand("u"), PUBKEY, null, { groupName: "" })).rejects.toMatchObject({ status: 400 });
    await expect(createRoom(rand("u"), PUBKEY, null, { groupName: "x".repeat(65) })).rejects.toMatchObject({ status: 400 });
    await expect(createRoom(rand("u"), PUBKEY, null, { groupDesc: "x".repeat(257) })).rejects.toMatchObject({ status: 400 });
    await expect(createRoom(rand("u"), PUBKEY, null, { maxMembers: 1 })).rejects.toMatchObject({ status: 400 });
    await expect(createRoom(rand("u"), PUBKEY, null, { maxMembers: 0 })).rejects.toMatchObject({ status: 400 });
    await expect(createRoom(rand("u"), PUBKEY, null, { maxMembers: 2.5 })).rejects.toMatchObject({ status: 400 });
    await expect(createRoom(rand("u"), PUBKEY, null, { parentCode: "abc" })).rejects.toMatchObject({ status: 400 });
    // boundary values are fine: groupDesc empty, maxMembers 2, groupName 64
    const g = await createRoom(rand("u"), PUBKEY, null, { groupDesc: "", maxMembers: 2, groupName: "x".repeat(64) });
    expect((await getRoomMeta(g.sessionId))?.maxMembers).toBe(2);
    expect((await getRoomMeta(g.sessionId))?.groupDesc).toBe("");
  });

  it("maxMembers rejects with the exact message; null = unlimited", async () => {
    const { sessionId } = await makeRoom({ maxMembers: 2 });
    const a = rand("a");
    const b = rand("b");
    await joinRoom(sessionId, a, PUBKEY); // creator + a = 2: full
    const err = await joinRoom(sessionId, b, PUBKEY).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RoomError);
    expect(err).toMatchObject({ status: 403 });
    expect((err as RoomError).message).toBe("Maximum allowance is reached");

    // unlimited default: no cap regardless of join count
    const open = await makeRoom();
    for (let i = 0; i < 5; i++) await joinRoom(open.sessionId, rand("m"), PUBKEY);
    expect((await getRoster(open.sessionId)).length).toBe(6); // creator + 5
  });

  it("crown transfer: creator leave promotes oldest admin, then oldest member", async () => {
    const { sessionId, username: creator } = await makeRoom();
    const admin1 = rand("a");
    const admin2 = rand("b");
    const member1 = rand("c");
    const member2 = rand("d");
    await joinRoom(sessionId, admin1, PUBKEY);
    await joinRoom(sessionId, admin2, PUBKEY);
    await joinRoom(sessionId, member1, PUBKEY);
    await joinRoom(sessionId, member2, PUBKEY);
    await setRole(sessionId, creator, admin1, "admin");
    await setRole(sessionId, creator, admin2, "admin");

    const creatorOf = async () => (await getRoomMeta(sessionId))?.creator;
    const roleOf = async (name: string) => (await getRoster(sessionId)).find((m) => m.username === name)?.role;
    expect(await creatorOf()).toBe(creator);

    // Creator leaves: the OLDEST admin (by join time) inherits, role and all.
    await leaveRoom(sessionId, creator);
    expect(await creatorOf()).toBe(admin1);
    expect(await roleOf(admin1)).toBe("creator");
    // The crown carries moderation power: grant + revoke as the new creator.
    await setRole(sessionId, admin1, member2, "admin");
    await setRole(sessionId, admin1, member2, "member");

    // admin1 leaves: the only remaining admin inherits.
    await leaveRoom(sessionId, admin1);
    expect(await creatorOf()).toBe(admin2);
    expect(await roleOf(admin2)).toBe("creator");

    // admin2 leaves: no admins left — the OLDEST member inherits.
    await leaveRoom(sessionId, admin2);
    expect(await creatorOf()).toBe(member1);
    expect(await roleOf(member1)).toBe("creator");

    // member1 leaves: member2 is the last survivor; then the room vanishes.
    await leaveRoom(sessionId, member1);
    expect(await creatorOf()).toBe(member2);
    const last = await leaveRoom(sessionId, member2);
    expect(last).toEqual({ remaining: 0, ended: true });
    expect(await roomExists(sessionId)).toBe(false);
  });
});

describe("group invites", () => {
  it("any member may invite; non-members cannot; existing/banned/self cannot be invited", async () => {
    const { sessionId, username: creator } = await makeRoom({ groupName: "Squad" });
    const alice = rand("a");
    const bob = rand("b");
    const kicked = rand("k");
    await joinRoom(sessionId, alice, PUBKEY);
    await joinRoom(sessionId, kicked, PUBKEY);
    await kickMember(sessionId, creator, kicked);

    // A plain member invites; the creator (also a member) may too.
    await createInvite(sessionId, alice, bob);
    const mine = await getInvitesForUser(bob);
    expect(mine).toEqual([
      { code: sessionId, groupName: "Squad", groupDesc: null, by: alice, at: expect.any(Number) },
    ]);

    // Non-members cannot invite.
    await expect(createInvite(sessionId, "stranger", rand("x"))).rejects.toMatchObject({ status: 403 });
    // Existing members cannot be invited.
    await expect(createInvite(sessionId, creator, alice)).rejects.toMatchObject({ status: 409 });
    // Banned users cannot be invited.
    await expect(createInvite(sessionId, creator, kicked)).rejects.toMatchObject({ status: 403 });
    // Self-invites are refused.
    await expect(createInvite(sessionId, alice, alice)).rejects.toMatchObject({ status: 400 });
    // Unknown room / bad invitee shapes.
    await expect(createInvite("999999", creator, bob)).rejects.toMatchObject({ status: 404 });
    await expect(createInvite(sessionId, creator, "ab")).rejects.toMatchObject({ status: 400 });

    // Idempotent re-invite refreshes {by, at} in place (no duplicate).
    const before = (await getInvitesForUser(bob))[0];
    await createInvite(sessionId, creator, bob);
    const after = (await getInvitesForUser(bob))[0];
    expect(after.by).toBe(creator);
    expect(after.at).toBeGreaterThanOrEqual(before.at);
    expect((await getInvitesForUser(bob)).length).toBe(1);
  });

  it("mine lists invites across rooms, prunes vanished rooms, and caps at 50", async () => {
    const target = rand("t");
    const r1 = await makeRoom({ groupName: "Alpha" });
    const r2 = await makeRoom({ groupName: "Beta", groupDesc: "the B room" });
    await createInvite(r1.sessionId, r1.username, target);
    await createInvite(r2.sessionId, r2.username, target);

    let mine = await getInvitesForUser(target);
    expect(mine).toHaveLength(2);
    expect(mine.map((i) => i.code).sort()).toEqual([r1.sessionId, r2.sessionId].sort());
    expect(mine.find((i) => i.code === r2.sessionId)).toMatchObject({
      groupName: "Beta",
      groupDesc: "the B room",
      by: r2.username,
      at: expect.any(Number),
    });

    // Decline one: it leaves the listing.
    await declineInvite(r1.sessionId, target);
    mine = await getInvitesForUser(target);
    expect(mine.map((i) => i.code)).toEqual([r2.sessionId]);

    // A room that died without destroy-cleanup is skipped AND pruned from
    // the per-user index on read (ROOM_INDEX-style pruning).
    const dead = await makeRoom();
    await createInvite(dead.sessionId, dead.username, target);
    await redis.del(`room:${dead.sessionId}`); // simulate TTL vanish
    mine = await getInvitesForUser(target);
    expect(mine.some((i) => i.code === dead.sessionId)).toBe(false);
    expect(await redis.hget(`user:${target}:invites`, dead.sessionId)).toBeNull();

    // Per-user cap: the 51st NEW invite is 429; re-invites pass at the cap.
    const host = rand("h");
    const cappedRooms: { sessionId: string; username: string }[] = [];
    for (let i = 0; i < MAX_PENDING_INVITES; i++) {
      const room = await makeRoom();
      await createInvite(room.sessionId, room.username, host);
      cappedRooms.push(room);
    }
    expect((await getInvitesForUser(host)).length).toBe(MAX_PENDING_INVITES);
    const extra = await makeRoom();
    await expect(createInvite(extra.sessionId, extra.username, host)).rejects.toMatchObject({ status: 429 });
    // Re-invite of an existing field at the cap still refreshes (no growth).
    await createInvite(cappedRooms[0].sessionId, cappedRooms[0].username, host);
    expect((await getInvitesForUser(host)).length).toBe(MAX_PENDING_INVITES);
  });

  it("accept joins and consumes the invite; unknown invites 404", async () => {
    const { sessionId, username: creator } = await makeRoom({ groupName: "Squad" });
    const invitee = rand("i");
    await createInvite(sessionId, creator, invitee);

    const { roster, epoch } = await acceptInvite(sessionId, invitee, PUBKEY);
    expect(roster.map((m) => m.username)).toContain(invitee);
    expect(epoch).toBeGreaterThan(0);
    expect((await getRoster(sessionId)).find((m) => m.username === invitee)?.role).toBe("member");

    // Consumed: gone from mine, re-accept and decline are 404.
    expect((await getInvitesForUser(invitee)).length).toBe(0);
    await expect(acceptInvite(sessionId, invitee, PUBKEY)).rejects.toMatchObject({ status: 404 });
    await expect(declineInvite(sessionId, invitee)).rejects.toMatchObject({ status: 404 });
    await expect(acceptInvite("999999", invitee, PUBKEY)).rejects.toMatchObject({ status: 404 });
  });

  it("accept on a full room 403s with the exact message AND consumes the invite", async () => {
    const { sessionId, username: creator } = await makeRoom({ maxMembers: 2 });
    const invitee = rand("i");
    const filler = rand("f");
    await createInvite(sessionId, creator, invitee);
    await joinRoom(sessionId, filler, PUBKEY); // creator + filler = full

    const err = await acceptInvite(sessionId, invitee, PUBKEY).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 403 });
    expect((err as RoomError).message).toBe("Maximum allowance is reached");
    // The invite was consumed and the invitee did NOT join.
    expect((await getRoster(sessionId)).map((m) => m.username)).not.toContain(invitee);
    expect((await getInvitesForUser(invitee)).length).toBe(0);
    await expect(acceptInvite(sessionId, invitee, PUBKEY)).rejects.toMatchObject({ status: 404 });
  });

  it("accept on a password room verifies the password FIRST (route-level 401s)", async () => {
    const creator = rand("u");
    const { sessionId } = await createRoom(creator, PUBKEY, await hashPassword("hunter2"));
    const invitee = rand("i");
    await createInvite(sessionId, creator, invitee);

    const call = async (password?: string) => {
      const req = new NextRequest(`http://localhost/api/v1/session/${sessionId}/invites/accept`, {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Uplink-Username": invitee },
        body: JSON.stringify({ code: sessionId, pubkey: PUBKEY, ...(password !== undefined ? { password } : {}) }),
      });
      return acceptInvitePOST(req, { params: Promise.resolve({ sessionId }) });
    };

    // No password / wrong password: 401 before anything else happens.
    let res = await call();
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe("Password is required for this session");
    res = await call("wrong");
    expect(res.status).toBe(401);
    expect((await res.json()).error).toBe("Incorrect session password");
    // Failed attempts leave the invite pending.
    expect((await getInvitesForUser(invitee)).length).toBe(1);

    // Correct password: joins (join-shaped response) and consumes.
    res = await call("hunter2");
    expect(res.status).toBe(200);
    const body = await res.json();
    expect(body.sessionId).toBe(sessionId);
    expect(body.participants).toContain(invitee);
    expect((await getInvitesForUser(invitee)).length).toBe(0);
  });

  it("decline consumes without joining; unknown invites 404", async () => {
    const { sessionId, username: creator } = await makeRoom();
    const invitee = rand("i");
    await createInvite(sessionId, creator, invitee);

    await declineInvite(sessionId, invitee);
    expect((await getInvitesForUser(invitee)).length).toBe(0);
    expect((await getRoster(sessionId)).map((m) => m.username)).not.toContain(invitee);
    expect(await redis.hget(`room:${sessionId}:invites`, invitee)).toBeNull();
    await expect(declineInvite(sessionId, invitee)).rejects.toMatchObject({ status: 404 });
    await expect(declineInvite("999999", invitee)).rejects.toMatchObject({ status: 404 });
  });

  it("room destruction cleans the invites hash and per-user index entries", async () => {
    const { sessionId, username: creator } = await makeRoom();
    const invitee = rand("i");
    const other = rand("o");
    await createInvite(sessionId, creator, invitee);
    await joinRoom(sessionId, other, PUBKEY);
    expect((await getInvitesForUser(invitee)).length).toBe(1);

    await leaveRoom(sessionId, creator);
    await leaveRoom(sessionId, other); // empty → destroy
    expect(await roomExists(sessionId)).toBe(false);
    expect(await redis.hgetall(`room:${sessionId}:invites`)).toBeNull();
    expect(await redis.hget(`user:${invitee}:invites`, sessionId)).toBeNull();
    expect((await getInvitesForUser(invitee)).length).toBe(0);
  });
});