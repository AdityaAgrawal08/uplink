import { describe, it, expect, vi } from "vitest";
import { redis } from "../../src/lib/redis";
import {
  RoomError,
  createRoom,
  joinRoom,
  getRoster,
  heartbeat,
  leaveRoom,
  kickMember,
  setRole,
  depositSignal,
  drainSignals,
  depositBox,
  fetchBoxes,
  ackBoxes,
  sweepRooms,
  roomExists,
  toggleReaction,
  fetchReactions,
  MAX_INBOX,
} from "../../src/lib/rooms";

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
    const { roster, epoch } = await joinRoom(sessionId, other, PUBKEY);
    expect(roster).toHaveLength(2);
    expect(epoch).toBeGreaterThan(0);
  });

  it("heartbeat refreshes presence and returns roster", async () => {
    const { sessionId, username } = await makeRoom();
    const { roster, epoch } = await heartbeat(sessionId, username, { peerId: "peer1", addrs: ["/ip4/1.2.3.4/tcp/1"] });
    expect(roster[0].peerId).toBe("peer1");
    expect(epoch).toBe(0); // no membership change since create
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
    let { boxes } = await fetchBoxes(sessionId, other);
    expect(boxes.map((b) => b.msgId).sort()).toEqual(["m1", "m2"]);
    // crash-before-ACK simulation: fetch again, boxes still there
    ({ boxes } = await fetchBoxes(sessionId, other));
    expect(boxes).toHaveLength(2);
    const ack = await ackBoxes(sessionId, other, ["m1"]);
    expect(ack.removed).toBe(1);
    ({ boxes } = await fetchBoxes(sessionId, other));
    expect(boxes.map((b) => b.msgId)).toEqual(["m2"]);
    await ackBoxes(sessionId, other, ["m2"]);
    expect((await fetchBoxes(sessionId, other)).boxes).toHaveLength(0);
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
    const { parseStored } = await import("../../src/lib/rooms");
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
    const { checkJoinLimit, checkReadLimit, checkSendLimit } = await import("../../src/lib/rooms");
    const ip = `test-ip-${Math.random().toString(36).slice(2)}`;
    // join budget: 120 per window
    for (let i = 0; i < 120; i++) await checkJoinLimit(ip);
    await expect(checkJoinLimit(ip)).rejects.toMatchObject({ status: 429 });
    // read budget covers two same-user tabs plus ack overhead
    const ip2 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 1200; i++) await checkReadLimit(ip2);
    await expect(checkReadLimit(ip2)).rejects.toMatchObject({ status: 429 });
    // send budget unchanged
    const ip3 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 120; i++) await checkSendLimit("sig", ip3);
    await expect(checkSendLimit("sig", ip3)).rejects.toMatchObject({ status: 429 });
  });

  it("rotatable budgets: fresh-username rotation from one IP still throttles (finding 6)", async () => {
    const { checkSendLimit, checkReadLimit, SEND_USER_SHARE_PER_WINDOW, READ_USER_SHARE_PER_WINDOW } = await import("../../src/lib/rooms");
    const ip = `test-ip-${Math.random().toString(36).slice(2)}`;

    // Per-username fairness share: a single user trips at 60 sends, far below
    // the 120 IP total.
    for (let i = 0; i < SEND_USER_SHARE_PER_WINDOW; i++) await checkSendLimit("sig", ip, "alice");
    await expect(checkSendLimit("sig", ip, "alice")).rejects.toMatchObject({ status: 429 });

    // Per-IP anti-Sybil total: rotating to FRESH usernames mints new user
    // buckets but the IP total is shared — the 121st send from this IP is
    // refused no matter which fresh name it rides.
    const ip2 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 120; i++) {
      await checkSendLimit("sig", ip2, `rotating_user_${i}`);
    }
    await expect(checkSendLimit("sig", ip2, "brand_new_user")).rejects.toMatchObject({ status: 429 });

    // Reads: same two-bucket structure.
    const ip3 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < READ_USER_SHARE_PER_WINDOW; i++) await checkReadLimit(ip3, "bob");
    await expect(checkReadLimit(ip3, "bob")).rejects.toMatchObject({ status: 429 });
    const ip4 = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 1200; i++) await checkReadLimit(ip4, `reader_${i}`);
    await expect(checkReadLimit(ip4, "fresh_reader")).rejects.toMatchObject({ status: 429 });

    // A different IP is unaffected.
    const ip5 = `test-ip-${Math.random().toString(36).slice(2)}`;
    await expect(checkSendLimit("sig", ip5, "alice")).resolves.toBeUndefined();
  });

  it("XFF trust: TRUST_PROXY=true keys budgets per IP, unset/false shares one bucket (finding 10)", async () => {
    const { clientIpHash, trustProxyHeader, checkSendLimit, SEND_LIMIT_PER_WINDOW } = await import("../../src/lib/rooms");
    const mkReq = (xff: string) => new Request("http://localhost/x", { headers: { "x-forwarded-for": xff } });

    // Default (TRUST_PROXY unset, no VERCEL): XFF is untrusted.
    delete process.env.TRUST_PROXY;
    delete process.env.VERCEL;
    expect(trustProxyHeader()).toBe(false);
    expect(clientIpHash(mkReq("1.2.3.4"))).toBe(clientIpHash(mkReq("9.9.9.9"))); // one shared bucket

    // Platform mode: VERCEL=1 trusts the platform's XFF even with the env unset.
    process.env.VERCEL = "1";
    expect(trustProxyHeader()).toBe(true);
    expect(clientIpHash(mkReq("1.2.3.4"))).not.toBe(clientIpHash(mkReq("9.9.9.9")));
    delete process.env.VERCEL;

    // Explicit "true": trusted — distinct XFF values get distinct buckets and
    // independent budgets.
    process.env.TRUST_PROXY = "true";
    expect(trustProxyHeader()).toBe(true);
    const ipA = clientIpHash(mkReq("1.2.3.4"));
    const ipB = clientIpHash(mkReq("9.9.9.9"));
    expect(ipA).not.toBe(ipB);
    for (let i = 0; i < SEND_LIMIT_PER_WINDOW; i++) await checkSendLimit("sig", ipA);
    await expect(checkSendLimit("sig", ipA)).rejects.toMatchObject({ status: 429 });
    await expect(checkSendLimit("sig", ipB)).resolves.toBeUndefined();
    delete process.env.TRUST_PROXY;

    // Explicit "false" (and any other non-true value): XFF ignored — both
    // "IPs" land in the same bucket and share the budget.
    process.env.TRUST_PROXY = "false";
    expect(trustProxyHeader()).toBe(false);
    const shared = clientIpHash(mkReq("1.2.3.4"));
    expect(clientIpHash(mkReq("9.9.9.9"))).toBe(shared);
    for (let i = 0; i < SEND_LIMIT_PER_WINDOW; i++) await checkSendLimit("sig", shared);
    await expect(checkSendLimit("sig", shared)).rejects.toMatchObject({ status: 429 });
    delete process.env.TRUST_PROXY;
  });

  it("password hash survives the meta roundtrip (room passwords stay enforced)", async () => {
    const username = `u_${Math.random().toString(36).slice(2, 10)}`;
    const { sessionId } = await createRoom(username, PUBKEY, "argon2id-fake-hash");
    const { getRoomMeta } = await import("../../src/lib/rooms");
    const meta = await getRoomMeta(sessionId);
    expect(meta?.passwordHash).toBe("argon2id-fake-hash");
    const open = await createRoom(`u_${Math.random().toString(36).slice(2, 10)}`, PUBKEY, null);
    const openMeta = await getRoomMeta(open.sessionId);
    expect(openMeta?.passwordHash).toBeNull();
  });
});

describe("delivery hardening", () => {
  it("drain preserves send order and empties the queue", async () => {
    const { sessionId, username } = await makeRoom();
    const peer = `p_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, peer, PUBKEY);
    await depositSignal(sessionId, username, peer, "offer", "sdp1");
    await depositSignal(sessionId, username, peer, "answer", "sdp2");
    const notes = await drainSignals(sessionId, peer);
    expect(notes.map((n) => n.payload)).toEqual(["sdp1", "sdp2"]);
    expect(await drainSignals(sessionId, peer)).toEqual([]);
  });

  it("same-sender idempotent retry bypasses a full inbox; cross-sender claim 409s", async () => {
    const { sessionId, username } = await makeRoom();
    const peer = `p_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, peer, PUBKEY);
    const big = "x".repeat(1024);
    for (let i = 0; i < MAX_INBOX; i++) {
      await depositBox(sessionId, username, peer, `m-${i}`, "p2p", big);
    }
    await expect(depositBox(sessionId, username, peer, "m-new", "p2p", big)).rejects.toMatchObject({ status: 429 });
    await depositBox(sessionId, username, peer, "m-0", "p2p", big); // same sender+id: retry succeeds
    const other = `o_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, other, PUBKEY);
    await expect(depositBox(sessionId, other, peer, "m-0", "p2p", big)).rejects.toMatchObject({ status: 409 });
  });

  it("inbox is byte-budgeted: MAX_INBOX × MAX_BOX_PAYLOAD ≈ 6.4MB worst case (finding 5)", async () => {
    const { MAX_BOX_PAYLOAD } = await import("../../src/lib/rooms");
    const { sessionId, username } = await makeRoom();
    const peer = `p_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, peer, PUBKEY);
    // Worst case per recipient is count-cap × payload-cap.
    expect(MAX_INBOX * MAX_BOX_PAYLOAD).toBeLessThanOrEqual(10 * 1024 * 1024);
    // Oversized single box: refused outright.
    await expect(depositBox(sessionId, username, peer, "huge", "p2p", "y".repeat(MAX_BOX_PAYLOAD + 1))).rejects.toMatchObject({ status: 400 });
    // Flood with max-size boxes fills the inbox…
    for (let i = 0; i < MAX_INBOX; i++) {
      await depositBox(sessionId, username, peer, `f-${i}`, "p2p", "y".repeat(MAX_BOX_PAYLOAD));
    }
    await expect(depositBox(sessionId, username, peer, "f-over", "p2p", "y".repeat(MAX_BOX_PAYLOAD))).rejects.toMatchObject({ status: 429 });
    // …but the victim can always ACK (the ACK path is never throttled) and
    // a legit small message still lands afterwards.
    await ackBoxes(sessionId, peer, ["f-0", "f-1"]);
    await depositBox(sessionId, username, peer, "legit-small", "p2p", "hello");
    const { boxes } = await fetchBoxes(sessionId, peer);
    expect(boxes.map((b) => b.msgId)).toContain("legit-small");
  });

  it("fetch caps at 50 and reaps corrupt fields", async () => {
    const { sessionId, username } = await makeRoom();
    const peer = `p_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, peer, PUBKEY);
    // Fill the inbox to its (byte-budgeted) cap of 50 via the API…
    for (let i = 0; i < MAX_INBOX; i++) {
      await depositBox(sessionId, username, peer, `c-${i}`, "p2p", "e30=");
    }
    // …then stuff it past the cap directly (bypassing depositBox, which
    // refuses): fetch must still return at most 50 boxes.
    for (let i = MAX_INBOX; i < MAX_INBOX + 10; i++) {
      await redis.hset(`room:${sessionId}:inbox:${peer}`, `c-${i}`, JSON.stringify({ msgId: `c-${i}`, from: username, kind: "p2p", payload: "e30=", ts: Date.now() }));
    }
    const { boxes } = await fetchBoxes(sessionId, peer);
    expect(boxes.length).toBe(50);
    expect(boxes[0].msgId).toBe("c-0");
  });

  it("roster epoch bumps on join/leave and rides heartbeat+inbox", async () => {
    const { sessionId, username } = await makeRoom();
    const other = `o_${Math.random().toString(36).slice(2, 10)}`;
    // Fresh room: epoch 0 everywhere.
    expect((await heartbeat(sessionId, username, {})).epoch).toBe(0);
    expect((await fetchBoxes(sessionId, username)).epoch).toBe(0);
    // Join bumps; joiner + heartbeat + inbox all report it.
    const joined = await joinRoom(sessionId, other, PUBKEY);
    expect(joined.epoch).toBe(1);
    expect((await heartbeat(sessionId, username, {})).epoch).toBe(1);
    expect((await fetchBoxes(sessionId, username)).epoch).toBe(1);
    // Leave bumps again; survivors see it without waiting for a beat.
    await leaveRoom(sessionId, other);
    expect((await heartbeat(sessionId, username, {})).epoch).toBe(2);
    expect((await fetchBoxes(sessionId, username)).epoch).toBe(2);
  });

  it("creator/admin/member ranks gate kick and grant", async () => {
    const { sessionId, username: creator } = await makeRoom();
    const admin = `a_${Math.random().toString(36).slice(2, 10)}`;
    const member = `m_${Math.random().toString(36).slice(2, 10)}`;
    const victim = `v_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, admin, PUBKEY);
    await joinRoom(sessionId, member, PUBKEY);
    await joinRoom(sessionId, victim, PUBKEY);

    const roles = async () => Object.fromEntries((await getRoster(sessionId)).map((m) => [m.username, m.role]));
    expect(await roles()).toMatchObject({ [creator]: "creator", [admin]: "member" });

    // Member cannot kick; admin cannot kick yet (not granted).
    await expect(kickMember(sessionId, member, victim)).rejects.toMatchObject({ status: 403 });
    await expect(kickMember(sessionId, admin, victim)).rejects.toMatchObject({ status: 403 });
    // Only the creator grants admin.
    await expect(setRole(sessionId, admin, member, "admin")).rejects.toMatchObject({ status: 403 });
    const granted = await setRole(sessionId, creator, admin, "admin");
    expect(granted.roster.find((m) => m.username === admin)?.role).toBe("admin");
    expect(granted.epoch).toBeGreaterThan(0);

    // A second admin: admins can never kick each other — only the creator can.
    await setRole(sessionId, creator, member, "admin");
    await expect(kickMember(sessionId, admin, member)).rejects.toMatchObject({ status: 403 });
    await expect(kickMember(sessionId, member, admin)).rejects.toMatchObject({ status: 403 });
    await setRole(sessionId, creator, member, "member");

    // Admin kicks members, never the creator.
    await expect(kickMember(sessionId, admin, creator)).rejects.toMatchObject({ status: 403 });
    const kicked = await kickMember(sessionId, admin, victim);
    expect(kicked.remaining).toBe(3);
    expect(kicked.roster.some((m) => m.username === victim)).toBe(false);
    // Kicked users stay out while the room lives.
    await expect(joinRoom(sessionId, victim, PUBKEY)).rejects.toMatchObject({ status: 403 });

    // Creator kicks admins too; nobody kicks themselves; creator role is immutable.
    await kickMember(sessionId, creator, admin);
    await expect(kickMember(sessionId, creator, creator)).rejects.toMatchObject({ status: 403 });
    await expect(setRole(sessionId, creator, creator, "admin")).rejects.toMatchObject({ status: 403 });
    await expect(setRole(sessionId, creator, member, "creator" as never)).rejects.toMatchObject({ status: 400 });

    // Revoke keeps them a member (not banned): re-grant works.
    await setRole(sessionId, creator, member, "admin");
    await setRole(sessionId, creator, member, "member");
    expect((await getRoster(sessionId)).find((m) => m.username === member)?.role).toBe("member");
  });

  it("ack reports skipped ids", async () => {
    const { sessionId, username } = await makeRoom();
    const peer = `p_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, peer, PUBKEY);
    await depositBox(sessionId, username, peer, "a-1", "p2p", "e30=");
    const res = await ackBoxes(sessionId, peer, ["a-1", 42, ""]);
    expect(res.removed).toBe(1);
    expect(res.skipped).toBe(2);
  });
});

describe("message reactions", () => {
  it("toggle adds, replaces own prior, and removes on repeated emoji", async () => {
    const { sessionId, username } = await makeRoom();
    const peer = `p_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, peer, PUBKEY);

    const on = await toggleReaction(sessionId, username, "m1", "👍");
    expect(on).toEqual({ msgId: "m1", emoji: "👍", reacted: true });
    let { reactions } = await fetchReactions(sessionId, username);
    expect(reactions).toEqual([
      { msgId: "m1", counts: { "👍": 1 }, mine: ["👍"], details: [{ emoji: "👍", usernames: [username] }] },
    ]);

    // Different emoji replaces the caller's prior one — it never stacks.
    const swapped = await toggleReaction(sessionId, username, "m1", "❤️");
    expect(swapped.reacted).toBe(true);
    ({ reactions } = await fetchReactions(sessionId, username));
    expect(reactions).toEqual([
      { msgId: "m1", counts: { "❤️": 1 }, mine: ["❤️"], details: [{ emoji: "❤️", usernames: [username] }] },
    ]);

    // Same emoji again toggles the caller's reaction off.
    const off = await toggleReaction(sessionId, username, "m1", "❤️");
    expect(off.reacted).toBe(false);
    ({ reactions } = await fetchReactions(sessionId, username));
    expect(reactions).toEqual([]);
  });

  it("stacks counts across users and reports mine per caller", async () => {
    const { sessionId, username: alice } = await makeRoom();
    const bob = `b_${Math.random().toString(36).slice(2, 10)}`;
    const carol = `c_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, bob, PUBKEY);
    await joinRoom(sessionId, carol, PUBKEY);

    await toggleReaction(sessionId, alice, "m1", "👍");
    await toggleReaction(sessionId, bob, "m1", "👍");
    await toggleReaction(sessionId, carol, "m1", "🙏");

    const fromAlice = await fetchReactions(sessionId, alice);
    expect(fromAlice.reactions).toEqual([
      {
        msgId: "m1",
        counts: { "👍": 2, "🙏": 1 },
        mine: ["👍"],
        details: [
          { emoji: "👍", usernames: [bob, alice].sort() },
          { emoji: "🙏", usernames: [carol] },
        ],
      },
    ]);
    const fromBob = await fetchReactions(sessionId, bob);
    expect(fromBob.reactions[0].counts).toEqual({ "👍": 2, "🙏": 1 });
    expect(fromBob.reactions[0].mine).toEqual(["👍"]);
    expect(fromBob.reactions[0].details).toEqual(fromAlice.reactions[0].details);
    const fromCarol = await fetchReactions(sessionId, carol);
    expect(fromCarol.reactions[0].mine).toEqual(["🙏"]);
  });

  it("keys counts by msgId across broadcast and DM id spaces", async () => {
    const { sessionId, username: alice } = await makeRoom();
    const bob = `b_${Math.random().toString(36).slice(2, 10)}`;
    const carol = `c_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, bob, PUBKEY);
    await joinRoom(sessionId, carol, PUBKEY);

    // Same room, same id space: a broadcast id and a DM id never share counts.
    await toggleReaction(sessionId, alice, "broadcast-1", "👍");
    await toggleReaction(sessionId, bob, "dm-alice-bob-1", "👍");
    await toggleReaction(sessionId, bob, "broadcast-1", "😂");

    const { reactions } = await fetchReactions(sessionId, alice);
    expect(reactions).toEqual([
      {
        msgId: "broadcast-1",
        counts: { "👍": 1, "😂": 1 },
        mine: ["👍"],
        details: [
          { emoji: "👍", usernames: [alice] },
          { emoji: "😂", usernames: [bob] },
        ],
      },
      { msgId: "dm-alice-bob-1", counts: { "👍": 1 }, mine: [], details: [{ emoji: "👍", usernames: [bob] }] },
    ]);

    // Documented limitation: details are room-scoped exactly like counts.
    // Carol is not a participant in the alice-bob DM, but the server cannot
    // know the msgId is DM-only (bodies are E2E ciphertext, IDs are
    // sender-assigned), so she sees its reactors too. Fixing this needs a
    // client-supplied conversation tag on the reaction itself.
    const fromCarol = await fetchReactions(sessionId, carol);
    expect(fromCarol.reactions.find((r) => r.msgId === "dm-alice-bob-1")?.details).toEqual([
      { emoji: "👍", usernames: [bob] },
    ]);
  });

  it("rejects bad emoji/msgId and non-members", async () => {
    const { sessionId, username } = await makeRoom();

    await expect(toggleReaction(sessionId, username, "m1", "👌")).rejects.toMatchObject({ status: 400 });
    await expect(toggleReaction(sessionId, username, "m1", "like")).rejects.toMatchObject({ status: 400 });
    await expect(toggleReaction(sessionId, username, "", "👍")).rejects.toMatchObject({ status: 400 });
    await expect(toggleReaction(sessionId, username, "x".repeat(129), "👍")).rejects.toMatchObject({ status: 400 });
    await expect(toggleReaction(sessionId, "stranger", "m1", "👍")).rejects.toMatchObject({ status: 403 });
    // Member-only gate covers the reactor breakdown too: a non-member can
    // never read who reacted (with or without a msgIds filter).
    await expect(fetchReactions(sessionId, "stranger")).rejects.toMatchObject({ status: 403 });
    await expect(fetchReactions(sessionId, "stranger", ["m1"])).rejects.toMatchObject({ status: 403 });
    await expect(fetchReactions("999999", username)).rejects.toMatchObject({ status: 404 });
    await expect(fetchReactions(sessionId, username, [])).rejects.toMatchObject({ status: 400 });
    await expect(fetchReactions(sessionId, username, ["x".repeat(129)])).rejects.toMatchObject({ status: 400 });
  });

  it("fetch caps at 50, filters by msgIds, and reaps corrupt fields", async () => {
    const { sessionId, username } = await makeRoom();
    for (let i = 0; i < 60; i++) {
      await toggleReaction(sessionId, username, `c-${i}`, "👍");
    }
    const capped = await fetchReactions(sessionId, username);
    expect(capped.reactions).toHaveLength(50);
    expect(capped.reactions[0].details).toEqual([{ emoji: "👍", usernames: [username] }]);

    const filtered = await fetchReactions(sessionId, username, ["c-59", "c-0"]);
    expect(filtered.reactions.map((r) => r.msgId).sort()).toEqual(["c-0", "c-59"]);
    expect(filtered.reactions[0].details).toEqual([{ emoji: "👍", usernames: [username] }]);

    // Corrupt fields (bad shape / bad emoji / bad username) are ignored and reaped.
    const key = `room:${sessionId}:reactions`;
    await redis.hset(key, "garbage|||", "1");
    await redis.hset(key, "m|🔥|alice", "1");
    await redis.hset(key, "m|👍|no spaces allowed", "1");
    const after = await fetchReactions(sessionId, username, ["c-0"]);
    expect(after.reactions).toHaveLength(1);
    expect(after.reactions[0].details).toEqual([{ emoji: "👍", usernames: [username] }]);
    expect(await redis.hget(key, "garbage|||")).toBeNull();
    expect(await redis.hget(key, "m|🔥|alice")).toBeNull();
    expect(await redis.hget(key, "m|👍|no spaces allowed")).toBeNull();
  });

  it("details sorts usernames ascending and caps at 20 per emoji without touching counts", async () => {
    const { sessionId, username: creator } = await makeRoom();
    const reactors = [creator];
    for (let i = 0; i < 21; i++) {
      const name = `r${String(i).padStart(2, "0")}`;
      await joinRoom(sessionId, name, PUBKEY);
      reactors.push(name);
    }
    // React in reverse join order: the response must still come back sorted
    // (stable UI order regardless of Redis hash iteration order).
    for (const name of [...reactors].reverse()) {
      await toggleReaction(sessionId, name, "m1", "👍");
    }

    const { reactions } = await fetchReactions(sessionId, creator);
    expect(reactions).toHaveLength(1);
    const [summary] = reactions;
    // 22 reactors, but counts stay the full total while details trims to 20.
    expect(summary.counts).toEqual({ "👍": 22 });
    expect(summary.details).toHaveLength(1);
    const [detail] = summary.details;
    expect(detail.emoji).toBe("👍");
    expect(detail.usernames).toHaveLength(20);
    const sorted = [...reactors].sort();
    expect(detail.usernames).toEqual(sorted.slice(0, 20));
    expect(detail.usernames).toEqual([...detail.usernames].sort()); // stable/ascending
    expect(detail.usernames[0]).toBe("r00");
  });

  it("departures clear the member's reactions", async () => {
    const { sessionId, username: alice } = await makeRoom();
    const bob = `b_${Math.random().toString(36).slice(2, 10)}`;
    await joinRoom(sessionId, bob, PUBKEY);
    await toggleReaction(sessionId, alice, "m1", "👍");
    await toggleReaction(sessionId, bob, "m1", "👍");
    let summary = (await fetchReactions(sessionId, alice)).reactions[0];
    expect(summary.counts).toEqual({ "👍": 2 });
    expect(summary.details).toEqual([{ emoji: "👍", usernames: [alice, bob].sort() }]);

    await leaveRoom(sessionId, bob);
    summary = (await fetchReactions(sessionId, alice)).reactions[0];
    expect(summary.counts).toEqual({ "👍": 1 });
    expect(summary.details).toEqual([{ emoji: "👍", usernames: [alice] }]);

    // Kick cleans up too.
    await joinRoom(sessionId, bob, PUBKEY);
    await toggleReaction(sessionId, bob, "m1", "😂");
    await kickMember(sessionId, alice, bob);
    summary = (await fetchReactions(sessionId, alice)).reactions[0];
    expect(summary.counts).toEqual({ "👍": 1 });
    expect(summary.details).toEqual([{ emoji: "👍", usernames: [alice] }]);
  });

  it("reaction sends trip the send budget with 429", async () => {
    const { checkSendLimit } = await import("../../src/lib/rooms");
    const ip = `test-ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 120; i++) await checkSendLimit("reactions", ip);
    await expect(checkSendLimit("reactions", ip)).rejects.toMatchObject({ status: 429 });
  });
});
