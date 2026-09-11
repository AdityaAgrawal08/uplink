import { NextRequest, NextResponse } from "next/server";
import fs from "fs";
import path from "path";
import { isMockStorage } from "@/lib/r2";

export async function GET(req: NextRequest) {
  try {
    // B35 FIX: only serve this dev-only filesystem shim when mock storage is
    // active, so it is never an open local file-read endpoint in production.
    if (!isMockStorage()) {
      return NextResponse.json({ error: "Not found" }, { status: 404 });
    }

    const { searchParams } = new URL(req.url);
    const key = searchParams.get("key");
    const preview = searchParams.get("preview") === "true";
    const rawFilename = searchParams.get("filename") || "file";
    const mimeType = searchParams.get("mimeType") || "application/octet-stream";

    // B35 FIX: sanitize the filename before interpolating it into
    // Content-Disposition. A crafted quote or CR/LF would otherwise corrupt
    // (or inject) response headers.
    const filename = rawFilename.replace(/[^a-zA-Z0-9._ -]/g, "_").slice(0, 200) || "file";

    if (!key) {
      return NextResponse.json({ error: "Missing key" }, { status: 400 });
    }

    if (key.includes("..")) {
      return NextResponse.json({ error: "Invalid key: path traversal attempt detected" }, { status: 400 });
    }

    const baseDir = path.resolve(process.cwd(), "uploads_dev");
    const localPath = path.resolve(baseDir, key);
    if (!localPath.startsWith(baseDir + path.sep)) {
      return NextResponse.json({ error: "Access denied: invalid path key" }, { status: 400 });
    }

    if (!fs.existsSync(localPath)) {
      return new Response("File Not Found", { status: 404 });
    }

    const stat = fs.statSync(localPath);
    const etag = `W/"${stat.size}-${stat.mtimeMs}"`;
    const ifNoneMatch = req.headers.get("if-none-match");
    if (ifNoneMatch === etag) {
      return new Response(null, { status: 304 });
    }

    const fileBuffer = fs.readFileSync(localPath);

    const headers = new Headers();
    headers.set("ETag", etag);
    if (preview) {
      headers.set("Content-Type", mimeType);
      headers.set("Content-Disposition", "inline");
    } else {
      headers.set("Content-Type", "application/octet-stream");
      headers.set("Content-Disposition", `attachment; filename="${filename}"`);
    }
    headers.set("Content-Length", String(fileBuffer.length));

    return new Response(fileBuffer, {
      status: 200,
      headers,
    });
  } catch (error: unknown) {
    console.error("Local mock R2 download failed:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return NextResponse.json({ error: errMsg }, { status: 500 });
  }
}
