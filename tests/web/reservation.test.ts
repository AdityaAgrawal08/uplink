import { describe, it, expect } from "vitest";
import { redis } from "../../src/lib/redis";
import {
  chargeIpReservation,
  releaseIpReservation,
  ipReservationKey,
} from "../../src/lib/reservation";

const MB = 1024 * 1024;

describe("per-IP reservation budget (share/init DoS)", () => {
  it("accepts charges under the cap and refuses over it, refunding the refused charge", async () => {
    const ip = `ip-${Math.random().toString(36).slice(2)}`;
    expect(await chargeIpReservation(ip, 500 * MB)).toBe(true);
    expect(await chargeIpReservation(ip, 500 * MB)).toBe(true); // exactly at cap: allowed
    // Third 500MB would exceed the ~1GB cap: refused AND refunded, so the
    // counter sits back at the two accepted charges (a refused request must
    // not poison the counter for later attempts).
    expect(await chargeIpReservation(ip, 500 * MB)).toBe(false);
    expect(Number(await redis.get(ipReservationKey(ip)))).toBe(2 * 500 * MB);
  });

  it("releases refund the counter; zero drops the key", async () => {
    const ip = `ip-${Math.random().toString(36).slice(2)}`;
    await chargeIpReservation(ip, 500 * MB);
    await chargeIpReservation(ip, 300 * MB);
    await releaseIpReservation(ip, 300 * MB);
    expect(Number(await redis.get(ipReservationKey(ip)))).toBe(500 * MB);
    await releaseIpReservation(ip, 500 * MB);
    expect(await redis.get(ipReservationKey(ip))).toBeNull(); // key dropped at zero
  });

  it("isolation: one IP hitting the cap does not affect another IP", async () => {
    const ipA = `ip-${Math.random().toString(36).slice(2)}`;
    const ipB = `ip-${Math.random().toString(36).slice(2)}`;
    for (let i = 0; i < 3; i++) await chargeIpReservation(ipA, 500 * MB); // 3rd refused
    expect(await chargeIpReservation(ipB, 500 * MB)).toBe(true);
    expect(Number(await redis.get(ipReservationKey(ipB)))).toBe(500 * MB);
  });

  it("invalid input is refused without touching the counter", async () => {
    const ip = `ip-${Math.random().toString(36).slice(2)}`;
    expect(await chargeIpReservation("", 500 * MB)).toBe(false);
    expect(await chargeIpReservation(ip, 0)).toBe(false);
    expect(await chargeIpReservation(ip, -5)).toBe(false);
    expect(await chargeIpReservation(ip, 1.5)).toBe(false);
    expect(await redis.get(ipReservationKey(ip))).toBeNull();
    await releaseIpReservation(ip, 0); // must not throw / must not create keys
    expect(await redis.get(ipReservationKey(ip))).toBeNull();
  });
});