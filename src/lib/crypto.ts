import crypto from "crypto";
import path from "path";
import { argon2id, argon2Verify } from "hash-wasm";

export async function hashPassword(password: string): Promise<string> {
  const salt = crypto.randomBytes(16);
  return argon2id({
    password,
    salt,
    iterations: 3,
    memorySize: 16384, // 16 MB
    parallelism: 1,
    hashLength: 32,
    outputType: "encoded",
  });
}

export async function verifyPassword(password: string, hash: string): Promise<boolean> {
  try {
    return await argon2Verify({ password, hash });
  } catch (error) {
    console.error("Argon2id password verification failed:", error);
    return false;
  }
}

let dummyPasswordHashPromise: Promise<string> | null = null;

// getDummyPasswordHash returns an argon2 hash of a fixed throwaway password,
// computed once per process. Routes verify against it when a room has NO
// password (finding 15): an unprotected room previously answered in ~1ms
// while a protected room burned 30-80ms on argon2 per wrong guess, so a
// remote scanner could map protected rooms by response timing without ever
// committing to a visible join. Verifying the (absent) password against the
// dummy hash equalizes the expensive work — the join still succeeds, this is
// a side-channel fix, not a new gate.
export function getDummyPasswordHash(): Promise<string> {
  if (!dummyPasswordHashPromise) {
    dummyPasswordHashPromise = hashPassword("uplink-timing-equalizer-dummy-password");
  }
  return dummyPasswordHashPromise;
}

let ipAnonymizationSecret: string;

function getIpAnonymizationSecret(): string {
  if (ipAnonymizationSecret) return ipAnonymizationSecret;

  const envSecret = process.env.IP_ANONYMIZATION_SECRET;
  if (!envSecret) {
    if (process.env.NODE_ENV === "production") {
      throw new Error("IP_ANONYMIZATION_SECRET environment variable is required in production");
    }
    // Development: generate ephemeral random secret
    ipAnonymizationSecret = crypto.randomBytes(32).toString("hex");
    console.warn("WARNING: IP_ANONYMIZATION_SECRET not set. Using ephemeral random secret (IPs will not be consistently anonymized across restarts).");
  } else {
    ipAnonymizationSecret = envSecret;
  }
  return ipAnonymizationSecret;
}

export function anonymizeIp(ip: string): string {
  const secret = getIpAnonymizationSecret();
  return crypto.createHmac("sha256", secret).update(ip).digest("hex");
}

export function generateShareId(): string {
  // Generate 16 secure random bytes (128 bits of entropy)
  // Encode as URL-safe base64 string (22 characters)
  return crypto
    .randomBytes(16)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}

export function sanitizeFilename(filename: string): string {
  // Extract basename to prevent path traversal. NOTE: the empty-string case
  // deliberately flows into the same "file" fallback used by the Go CLI
  // (cli/main.go sanitizeFilename) so both ends agree on every input.
  const base = path.basename(String(filename ?? ""));
  const sanitized = base.replace(/[^a-zA-Z0-9._-]/g, "_");
  if (!sanitized || sanitized === "." || sanitized === "..") {
    return "file";
  }
  return sanitized;
}
