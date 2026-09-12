const requiredVars = [
  "MONGODB_URI",
  "R2_ACCESS_KEY_ID",
  "R2_SECRET_ACCESS_KEY",
  "R2_ENDPOINT_URL",
  "R2_BUCKET_NAME",
  "UPSTASH_REDIS_REST_URL",
  "UPSTASH_REDIS_REST_TOKEN",
  "IP_ANONYMIZATION_SECRET",
];

// Signaling routes (chat rooms) never touch Mongo/R2 — they need only the
// Redis backend plus the IP-hash secret. Gating them on the full list would
// 500 a lean signaling-only deploy that intentionally omits Mongo/R2.
const signalingVars = [
  "UPSTASH_REDIS_REST_URL",
  "UPSTASH_REDIS_REST_TOKEN",
  "IP_ANONYMIZATION_SECRET",
];

let validated = false;
let signalingValidated = false;

function check(vars: string[]): void {
  if (process.env.NODE_ENV !== "production") {
    const missing = vars.filter(v => !process.env[v]);
    if (missing.length > 0) {
      console.warn(
        `[env] Missing environment variables (running in dev/mock mode): ${missing.join(", ")}`
      );
    }
    return;
  }
  const missing = vars.filter(v => !process.env[v]);
  if (missing.length > 0) {
    throw new Error(`Missing required environment variables: ${missing.join(", ")}`);
  }
}

export function validateEnv(): void {
  if (validated) return;
  check(requiredVars);

  // B23 FIX: In development, warn about missing vars instead of silently
  // proceeding. Previously there was no feedback at all in dev, making it
  // hard to diagnose why features silently degraded to mock mode.
  validated = true;
}

// validateSignalingEnv gates the chat signaling plane: without a real Redis
// backend, production instances would silently fall back to per-instance
// MockRedis and rooms would split-brain across serverless instances.
export function validateSignalingEnv(): void {
  if (signalingValidated) return;
  check(signalingVars);
  signalingValidated = true;
}
