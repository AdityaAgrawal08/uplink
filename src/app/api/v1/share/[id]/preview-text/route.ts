import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { getObjectText } from "@/lib/r2";
import { verifyPassword } from "@/lib/crypto";
import { apiError } from "@/lib/api-utils";

export async function POST(
  req: NextRequest,
  props: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await props.params;
    const body = await req.json().catch(() => ({}));
    const { password } = body;

    const db = await getDb();
    const share = (await db.collection("shares").findOne({ $or: [{ shareId: id }, { downloadCode: id }] })) as {
      shareId: string;
      status: string;
      passwordHash?: string;
      objectKey: string;
      isEncrypted?: boolean;
    } | null;

    if (!share) {
      return apiError("Share not found", 404);
    }

    if (share.status !== "ACTIVE") {
      return apiError("Share is not active", 400);
    }

    // Encrypted shares hold ciphertext — text preview would only leak noise.
    if ((share as { isEncrypted?: boolean }).isEncrypted) {
      return apiError("Encrypted shares cannot be previewed as text", 400);
    }

    if (share.passwordHash) {
      if (!password) {
        return NextResponse.json({ error: "Password required" }, { status: 401 });
      }
      const isValid = await verifyPassword(password, share.passwordHash);
      if (!isValid) {
        return NextResponse.json({ error: "Incorrect password" }, { status: 401 });
      }
    }

    const text = await getObjectText(share.objectKey);
    return NextResponse.json({ text: text.slice(0, 100000) });
  } catch (err: unknown) {
    const errMsg = err instanceof Error ? err.message : "Internal Server Error";
    return apiError(errMsg, 500);
  }
}
