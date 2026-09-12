import { NextRequest, NextResponse } from "next/server";
import { getDb } from "@/lib/mongodb";
import { s3Client } from "@/lib/r2";
import { ListPartsCommand } from "@aws-sdk/client-s3";
import { crc64nvmeBase64 } from "@/lib/crc64";
import fs from "fs";
import path from "path";

interface ResponsePart {
  partNumber: number;
  etag: string;
  checksum?: string;
}

export async function GET(
  req: NextRequest,
  props: { params: Promise<{ id: string }> }
) {
  try {
    const { id } = await props.params;
    const db = await getDb();

    // B6 FIX: Require uploadId as proof of ownership. Previously anyone who
    // knew a shareId could enumerate upload parts without authentication.
    const uploadIdParam = req.nextUrl.searchParams.get("uploadId");
    if (!uploadIdParam) {
      return NextResponse.json({ error: "uploadId query parameter is required" }, { status: 401 });
    }

    // 1. Find the share
    const share = await db.collection("shares").findOne({ $or: [{ shareId: id }, { downloadCode: id }] });
    if (!share) {
      return NextResponse.json({ error: "Share not found" }, { status: 404 });
    }

    // 2. Find the upload session — must match the provided uploadId
    const session = await db.collection("upload_sessions").findOne({ shareId: share.shareId, uploadId: uploadIdParam });
    if (!session) {
      return NextResponse.json({ error: "Upload session not found or uploadId mismatch" }, { status: 404 });
    }

    let parts: ResponsePart[] = [];

    if (session.isMultipart) {
      const uploadId = session.uploadId;
      const objectKey = share.objectKey;

      if (process.env.R2_ENDPOINT_URL) {
        // Real S3/R2 mode: List parts from S3/R2
        const command = new ListPartsCommand({
          Bucket: process.env.R2_BUCKET_NAME,
          Key: objectKey,
          UploadId: uploadId,
        });
        if (!s3Client) {
          throw new Error("S3 Client is not initialized");
        }
        const res = await s3Client.send(command);
        parts = res.Parts?.map(p => ({
          partNumber: p.PartNumber || 0,
          etag: p.ETag || "",
          checksum: p.ChecksumCRC64NVME || "",
        })).filter(p => p.partNumber > 0) || [];
      } else {
        // Mock mode: List parts from uploads_dev directory
        const baseDir = path.resolve(process.cwd(), "uploads_dev");
        const fileDir = path.join(baseDir, path.dirname(objectKey));
        if (fs.existsSync(fileDir)) {
          const files = fs.readdirSync(fileDir);
          const baseName = path.basename(objectKey);
          for (const file of files) {
            if (file.startsWith(baseName + ".part_")) {
              const partNumStr = file.substring((baseName + ".part_").length);
              const partNum = parseInt(partNumStr, 10);
              if (!isNaN(partNum)) {
                const partPath = path.join(fileDir, file);
                const fileContent = fs.readFileSync(partPath);
                const checksum = crc64nvmeBase64(fileContent);
                parts.push({
                  partNumber: partNum,
                  etag: `"${uploadId}-${partNum}"`,
                  checksum: checksum,
                });
              }
            }
          }
        }
      }
    }

    return NextResponse.json({
      uploadId: session.uploadId,
      parts: parts.sort((a, b) => a.partNumber - b.partNumber),
    });
  } catch (error: unknown) {
    console.error("Error listing parts for resume:", error);
    const errMsg = error instanceof Error ? error.message : "Internal Server Error";
    return NextResponse.json({ error: errMsg }, { status: 500 });
  }
}
