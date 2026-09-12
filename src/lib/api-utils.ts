import { NextResponse } from "next/server";

export function apiError(message: string, status: number, extra?: Record<string, unknown>) {
  return NextResponse.json({ error: message, ...extra }, { status });
}

// parseJsonBody safely reads a request body that is expected to be a JSON
// object. Returns { ok:false } (instead of throwing) so route handlers can
// answer 400 Bad Request rather than leaking a 500 for client mistakes.
export async function parseJsonBody(req: Request): Promise<{ ok: true; body: Record<string, unknown> } | { ok: false }> {
  let text: string;
  try {
    text = await req.text();
  } catch {
    return { ok: false };
  }
  if (!text) return { ok: true, body: {} };
  try {
    const parsed = JSON.parse(text);
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      return { ok: false };
    }
    return { ok: true, body: parsed as Record<string, unknown> };
  } catch {
    return { ok: false };
  }
}
