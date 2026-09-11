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

let validated = false;

export function validateEnv(): void {
  if (validated) return;

  // B23 FIX: In development, warn about missing vars instead of silently
  // proceeding. Previously there was no feedback at all in dev, making it
  // hard to diagnose why features silently degraded to mock mode.
  if (process.env.NODE_ENV !== "production") {
    const missing = requiredVars.filter(v => !process.env[v]);
    if (missing.length > 0) {
      console.warn(
        `[env] Missing environment variables (running in dev/mock mode): ${missing.join(", ")}`
      );
    }
    validated = true;
    return;
  }

  const missing = requiredVars.filter(v => !process.env[v]);
  if (missing.length > 0) {
    throw new Error(`Missing required environment variables: ${missing.join(", ")}`);
  }
  validated = true;
}
