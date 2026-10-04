import { redis } from "./redis";

// Per-IP in-flight upload reservation budget (anti-DoS, Redis-backed).
//
// share/init reserves R2 quota in Mongo AND a per-IP byte budget here. The
// Redis counter is keyed on the anonymized client-IP hash, is charged at
// reserve time, and is refunded on every path that ends the reservation
// (init error, confirm success or failure, expired-session cleanup — the
// initiator's ipHash is stored on the share doc so confirm/cleanup can
// refund the right counter without a request). A single IP therefore cannot
// hold more than MAX_IP_RESERVED_BYTES of pending uploads at once, so an
// attacker cannot carpet-bomb init with many large sizes to pin the storage
// reservation ledger (finding 4).
//
// The 2h TTL is a deliberate backstop: a crash between charge and refund
// decays the counter on its own, aligned with the upload-session lifetime.

export const MAX_IP_RESERVED_BYTES = 1024 * 1024 * 1024; // ~1 GB of in-flight reservations per IP
export const IP_RESERVATION_TTL_SEC = 2 * 3600; // 2h backstop

export const ipReservationKey = (ipHash: string) => `resv:ip:${ipHash}`;

// Charge `fileSize` bytes against the IP's in-flight reservation budget.
// Returns false (and refunds the charge) when the budget is exceeded.
export async function chargeIpReservation(ipHash: string, fileSize: number): Promise<boolean> {
  if (!ipHash || typeof fileSize !== "number" || !Number.isSafeInteger(fileSize) || fileSize <= 0) {
    return false;
  }
  const key = ipReservationKey(ipHash);
  const total = await redis.incrBy(key, fileSize);
  if (total === fileSize) {
    await redis.expire(key, IP_RESERVATION_TTL_SEC); // first charge arms the backstop
  }
  if (total > MAX_IP_RESERVED_BYTES) {
    // Refuse AND refund this charge: the counter must not sit above the cap
    // after a refused request (one refused 500MB init would otherwise poison
    // every later attempt from this IP until the TTL).
    await redis.incrBy(key, -fileSize).catch(() => {});
    return false;
  }
  return true;
}

// Refund `fileSize` bytes of the IP's in-flight reservation (reservation
// ended: committed, aborted, or expired). When the counter reaches zero the
// key is dropped so the next charge starts from a clean 0. The known race —
// a fresh charge landing between the incrBy and the del — costs at most one
// dropped charge for that IP and self-heals via the 2h TTL.
export async function releaseIpReservation(ipHash: string, fileSize: number): Promise<void> {
  if (!ipHash || typeof fileSize !== "number" || !Number.isSafeInteger(fileSize) || fileSize <= 0) return;
  const key = ipReservationKey(ipHash);
  const total = await redis.incrBy(key, -fileSize);
  if (total <= 0) {
    await redis.del(key).catch(() => {});
  }
}