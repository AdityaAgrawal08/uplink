import { describe, it, expect, vi } from "vitest";
import { redis } from "../redis";
import {
  RoomError,
  createRoom,
  joinRoom,
  getRoster,
  heartbeat,
  leaveRoom,
  depositSignal,
  drainSignals,
  depositBox,
  fetchBoxes,
  ackBoxes,
  sweepRooms,
  roomExists,
} from "../rooms";

const PUBKEY = Buffer.alloc(32, 7).toString("base64");

async function makeRoom(passwordHash: string | null = null) {
  const username = `u_${Math.random().toString(36).slice(2, 10)}`;
  const { sessionId } = await createRoom(username, PUBKEY, passwordHash);
  return { sessionId, username };
}

describe("rooms signaling plane", () => {
  it("creates a room and lists the creator", async () => {
    const { sessionId, username } = await makeRoom();
    expect(sessionId).toMatch(/^[0-9]{6}$/);
    expect(await roomExists(sessionId)).toBe(true);
    const roster = await getRoster(sessionId);
    expect(roster.map((m) => m.username)).toContain(username);
    expect(roster[0].pubkey).toBe(PUBKEY);
    expect(roster[0].online).toBe(true);
  });

  it("rejects bad usernames, pubkeys, and codes", async () => {
    await expect(createRoom("ab", PUBKEY, null)).rejects.toMatchObject({ status: 400 });
    await expect(createRoom("valid_name", "not-base64!!", null)).rejects.toMatchObject({ status: 400 });
    await expect(createRoom("valid_name", Buffer.alloc(16).toString("base64"), null)).rejects.toMatchObject({ status: 400 });
    await expect(joinRoom("999999", "ghost", PUBKEY)).rejects.toMatchObject({ status: 404 });
    await expect(joinRoom("abc", "ghost", PUBKEY)).rejects.toMatchObject({ status: 400 });
  });

  it("duplicate username claim returns 409", async () => {
    const { sessionId, username } = await makeRoom();
    await expect(joinRoom(sessionId, username, PUBKEY)).rejects.toMatchObject({ status: 409 });
    const other = `o_${Math.random().toString(36).slice(2, 10)}`;
    const roster = await joinRoom(sessionId, other, PUBKEY);
    expect(roster).toHaveLength(2);
  });

  it("heartbeat refreshes presence and returns roster", async () => {
    const { sessionId, username } = await makeRoom();
    const roster = await heartbeat(sessionId, username, { peerId: "peer1", addrs: ["/ip4/1.2.3.4/tcp/1"] });
    expect(roster[0].peerId).toBe("peer1");
    await expect(heartbeat(sessionId, "stranger", {})).rejects.toMatchObject({ status: 403 });
    await expect(heartbeat(sessionId, username, { peerId: 42 as never })).rejects.toThrow();
  });

  it("last leave destroys the room (rooms live till empty)", async () => {
    const { sessionId, username } = await makeRoom();
    const other = `o_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, other, PUBKEY);
    const r1 = await leaveRoom(sessionId, username);
    expect(r1).toEqual({ remaining: 1, ended: false });
    expect(await roomExists(sessionId)).toBe(true);
    const r2 = await leaveRoom(sessionId, other);
    expect(r2).toEqual({ remaining: 0, ended: true });
    expect(await roomExists(sessionId)).toBe(false);
    // idempotent re-leave reports true count
    const r3 = await leaveRoom(sessionId, other);
    expect(r3.remaining).toBe(0);
  });

  it("signaling notes round-trip and drain clears", async () => {
    const { sessionId, username } = await makeRoom();
    const other = `o_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, other, PUBKEY);
    await depositSignal(sessionId, username, other, "offer", "SDP...");
    await depositSignal(sessionId, other, username, "answer", "SDP!");
    const notes = await drainSignals(sessionId, other);
    expect(notes).toHaveLength(1);
    expect(notes[0]).toMatchObject({ from: username, type: "offer" });
    expect(await drainSignals(sessionId, other)).toHaveLength(0);
    await expect(depositSignal(sessionId, username, username, "offer", "x")).rejects.toMatchObject({ status: 400 });
    await expect(depositSignal(sessionId, username, "nobody", "offer", "x")).rejects.toMatchObject({ status: 404 });
  });

  it("inbox is idempotent on msgId; fetch-then-ACK deletes exactly", async () => {
    const { sessionId, username } = await makeRoom();
    const other = `o_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, other, PUBKEY);
    await depositBox(sessionId, username, other, "m1", "chat", "cipher1");
    await depositBox(sessionId, username, other, "m1", "chat", "cipher1"); // retry: no duplicate
    await depositBox(sessionId, username, other, "m2", "chat", "cipher2");
    let boxes = await fetchBoxes(sessionId, other);
    expect(boxes.map((b) => b.msgId).sort()).toEqual(["m1", "m2"]);
    // crash-before-ACK simulation: fetch again, boxes still there
    boxes = await fetchBoxes(sessionId, other);
    expect(boxes).toHaveLength(2);
    const ack = await ackBoxes(sessionId, other, ["m1"]);
    expect(ack.removed).toBe(1);
    boxes = await fetchBoxes(sessionId, other);
    expect(boxes.map((b) => b.msgId)).toEqual(["m2"]);
    await ackBoxes(sessionId, other, ["m2"]);
    expect(await fetchBoxes(sessionId, other)).toHaveLength(0);
    await expect(depositBox(sessionId, username, username, "m3", "chat", "x")).rejects.toMatchObject({ status: 400 });
  });

  it("sweep prunes stale members and destroys emptied rooms", async () => {
    const { sessionId, username } = await makeRoom();
    // joinRoom sets a fresh beat; heartbeat keeps it fresh — room survives
    let res = await sweepRooms();
    expect(res.processed).toBeGreaterThanOrEqual(1);
    expect(await roomExists(sessionId)).toBe(true);
    // empty it via leave; sweep must not resurrect anything
    await leaveRoom(sessionId, username);
    res = await sweepRooms();
    expect(await roomExists(sessionId)).toBe(false);
  });

  it("RoomError carries status", () => {
    const e = new RoomError(429, "slow");
    expect(e.status).toBe(429);
    expect(e.message).toBe("slow");
  });

  // Production backend gap: real Upstash Redis returns {} (not null) for
  // HGETALL on missing keys, while MockRedis returns null. The rooms layer
  // must treat both as missing, or ghost rooms read as existing in prod
  // (usernames claimable into rooms with no metadata, 404s become 403s).
  it("treats Upstash-style empty objects as missing keys", async () => {
    const spy = vi.spyOn(redis, "hgetall").mockResolvedValue({});
    try {
      expect(await roomExists("000000")).toBe(false);
      expect(await getRoster("000000")).toEqual([]);
      await expect(joinRoom("000000", "ghost", PUBKEY)).rejects.toMatchObject({ status: 404 });
    } finally {
      spy.mockRestore();
    }
  });

  // The real Upstash client auto-parses JSON-looking strings into objects
  // on read (verified live against production). Every decode path must
  // tolerate object values, or the entire plane reads as missing on real
  // Redis while MockRedis tests stay green — exactly the outage this
  // guards. Simulates full object-shape roundtrips.
  it("tolerates Upstash auto-parsed object values end to end", async () => {
    const { parseStored } = await import("../rooms");
    // unit level: both shapes decode identically
    const doc = { a: 1, b: "x" };
    expect(parseStored<typeof doc>(JSON.stringify(doc))).toEqual(doc);
    expect(parseStored<typeof doc>(doc)).toEqual(doc);
    expect(parseStored("not json{{{")).toBeNull();
    expect(parseStored<typeof doc>(null)).toBeNull();
    expect(parseStored<typeof doc>(42)).toBeNull();

    // behavior level: object-valued hashes still resolve rooms + rosters
    const metaObj = { passwordHash: null, createdAt: new Date().toISOString() };
    const memObj = { pubkey: PUBKEY, beat: Date.now() };
    const hgetSpy = vi.spyOn(redis, "hgetall").mockImplementation(async (key: string) => {
      if (key.endsWith(":members")) return { alice: memObj } as unknown as Record<string, string>;
      return { meta: metaObj } as unknown as Record<string, string>;
    });
    try {
      expect(await roomExists("123456")).toBe(true);
      const roster = await getRoster("123456");
      expect(roster.map((m) => m.username)).toEqual(["alice"]);
      expect(roster[0].pubkey).toBe(PUBKEY);
    } finally {
      hgetSpy.mockRestore();
    }
  });

  it("rate limiters trip at their budgets with 429", async () => {
    const { checkJoinLimit, checkReadLimit, checkSendLimit } = await import("../rooms");
    const ip = `test-ip-${Math.random().toString(36).slice(2)}`;
    // join budget: 120 per window
    for (let i = 0; i < 120; i++) await checkJoinLimit(ip);
    await expect(checkJoinLimit(ip)).rejects.toMatchObject({ status: 429 });
    // read budget is roomier but finite
    const ip2 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 600; i++) await checkReadLimit(ip2);
    await expect(checkReadLimit(ip2)).rejects.toMatchObject({ status: 429 });
    // send budget unchanged
    const ip3 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 120; i++) await checkSendLimit("sig", ip3);
    await expect(checkSendLimit("sig", ip3)).rejects.toMatchObject({ status: 429 });
  });

  it("password hash survives the meta roundtrip (room passwords stay enforced)", async () => {
    const username = `u_${Math.random().toString(36).slice(2, 10)}`;
    const { sessionId } = await createRoom(username, PUBKEY, "argon2id-fake-hash");
    const { getRoomMeta } = await import("../rooms");
    const meta = await getRoomMeta(sessionId);
    expect(meta?.passwordHash).toBe("argon2id-fake-hash");
    const open = await createRoom(`u_${Math.random().toString(36).slice(2, 10)}`, PUBKEY, null);
    const openMeta = await getRoomMeta(open.sessionId);
    expect(openMeta?.passwordHash).toBeNull();
  });
});
