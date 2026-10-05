import {
  S3Client,
  PutObjectCommand,
  GetObjectCommand,
  DeleteObjectCommand,
  HeadObjectCommand,
  CreateMultipartUploadCommand,
  UploadPartCommand,
  CompleteMultipartUploadCommand,
} from "@aws-sdk/client-s3";
import { getSignedUrl } from "@aws-sdk/s3-request-presigner";
import fs from "fs";
import path from "path";
import crypto from "crypto";
import { crc64nvmeBase64 } from "./crc64";

const accessKeyId = process.env.R2_ACCESS_KEY_ID;
const secretAccessKey = process.env.R2_SECRET_ACCESS_KEY;
const endpoint = process.env.R2_ENDPOINT_URL;
const bucketName = process.env.R2_BUCKET_NAME;

export let s3Client: S3Client | null = null;

export function isMockStorage(): boolean {
  return !s3Client || process.env.FORCE_MOCK_STORAGE === "true";
}

const isPlaceholder = (val: string | undefined) => {
  if (!val) return true;
  return /<.*>|_here|placeholder|example|your_|xxxx|test_/i.test(val);
};

if (
  process.env.FORCE_MOCK_STORAGE !== "true" &&
  accessKeyId &&
  secretAccessKey &&
  endpoint &&
  !isPlaceholder(accessKeyId) &&
  !isPlaceholder(secretAccessKey) &&
  !isPlaceholder(endpoint)
) {
  try {
    s3Client = new S3Client({
      region: "auto",
      endpoint: endpoint,
      credentials: {
        accessKeyId: accessKeyId,
        secretAccessKey: secretAccessKey,
      },
    });
  } catch (err) {
    console.error("Failed to initialize S3 client, falling back to mock mode:", err);
    s3Client = null;
  }
} else {
  console.log("Cloudflare R2 credentials missing or contain placeholders. Running in local dev file mock mode.");
}

export function getBucketName() {
  return bucketName || "uplink-bucket";
}

export async function getPresignedUploadUrl(
  objectKey: string,
  expiresInSeconds: number,
  mimeType: string,
  hashValue: string // SHA-256
): Promise<string> {
  if (!s3Client || process.env.FORCE_MOCK_STORAGE === "true") {
    // Local dev mock route
    return `${process.env.APP_URL || "http://localhost:3000"}/api/v1/mock-r2-upload?key=${encodeURIComponent(objectKey)}&sha256=${encodeURIComponent(hashValue)}&mimeType=${encodeURIComponent(mimeType)}`;
  }

  const command = new PutObjectCommand({
    Bucket: getBucketName(),
    Key: objectKey,
    ContentType: mimeType,
  });

  return getSignedUrl(s3Client, command, { expiresIn: expiresInSeconds });
}

export async function getPresignedDownloadUrl(
  objectKey: string,
  expiresInSeconds: number,
  storageFilename: string,
  mimeType: string,
  preview: boolean
): Promise<string> {
  if (!s3Client || process.env.FORCE_MOCK_STORAGE === "true") {
    // Local dev mock route
    return `${process.env.APP_URL || "http://localhost:3000"}/api/v1/mock-r2-download?key=${encodeURIComponent(objectKey)}&preview=${preview}&filename=${encodeURIComponent(storageFilename)}&mimeType=${encodeURIComponent(mimeType)}`;
  }

  const contentDisposition = preview
    ? "inline"
    : `attachment; filename="${storageFilename}"`;
  
  const contentType = preview ? mimeType : "application/octet-stream";

  const command = new GetObjectCommand({
    Bucket: getBucketName(),
    Key: objectKey,
    ResponseContentDisposition: contentDisposition,
    ResponseContentType: contentType,
  });

  return getSignedUrl(s3Client, command, { expiresIn: expiresInSeconds });
}

export interface ObjectDetails {
  size: number;
  checksumSha256?: string;
  checksumCrc64nvme?: string;
  contentType?: string;
  exists: boolean;
  error?: string;
}

export async function checkObjectExists(objectKey: string): Promise<ObjectDetails> {
  if (!s3Client || process.env.FORCE_MOCK_STORAGE === "true") {
    // Mock local dev file check
    const localPath = path.join(process.cwd(), "uploads_dev", objectKey);
    if (fs.existsSync(localPath)) {
      const stats = fs.statSync(localPath);
      // Retrieve stored mock checksum if available
      let checksum = "";
      let checksumCrc64nvme = "";
      try {
        const metaPath = localPath + ".meta";
        if (fs.existsSync(metaPath)) {
          const meta = JSON.parse(fs.readFileSync(metaPath, "utf-8"));
          checksum = meta.sha256;
          checksumCrc64nvme = meta.checksumCrc64nvme || "";
        }
      } catch {}
      return {
        size: stats.size,
        checksumSha256: checksum || "mock_sha256_hash",
        checksumCrc64nvme: checksumCrc64nvme || undefined,
        exists: true,
      };
    }
    return { size: 0, exists: false, error: "Mock file not found locally" };
  }

  try {
    const command = new HeadObjectCommand({
      Bucket: getBucketName(),
      Key: objectKey,
    });
    const response = await s3Client.send(command);
    return {
      size: response.ContentLength || 0,
      checksumSha256: response.ChecksumSHA256,
      checksumCrc64nvme: response.ChecksumCRC64NVME,
      contentType: response.ContentType,
      exists: true,
    };
  } catch (error: unknown) {
    const err = error as { name?: string; $metadata?: { httpStatusCode?: number }; message?: string };
    if (err.name === "NotFound" || err.$metadata?.httpStatusCode === 404) {
      return { size: 0, exists: false, error: "Object not found" };
    }
    return { size: 0, exists: false, error: err.message || "Error checking object" };
  }
}

export async function deleteObject(objectKey: string): Promise<boolean> {
  if (!s3Client || process.env.FORCE_MOCK_STORAGE === "true") {
    const localPath = path.join(process.cwd(), "uploads_dev", objectKey);
    try {
      if (fs.existsSync(localPath)) {
        fs.unlinkSync(localPath);
      }
      const metaPath = localPath + ".meta";
      if (fs.existsSync(metaPath)) {
        fs.unlinkSync(metaPath);
      }
      return true;
    } catch (err) {
      console.error("Local mock delete failed:", err);
      return false;
    }
  }

  try {
    const command = new DeleteObjectCommand({
      Bucket: getBucketName(),
      Key: objectKey,
    });
    await s3Client.send(command);
    return true;
  } catch (error) {
    console.error(`Failed to delete object ${objectKey} from R2:`, error);
    return false;
  }
}

export async function getPresignedMultipartUrls(
  objectKey: string,
  partsCount: number,
  uploadIdFromReq: string | null = null
): Promise<{ uploadId: string; urls: string[] }> {
  if (!s3Client || process.env.FORCE_MOCK_STORAGE === "true") {
    const uploadId = uploadIdFromReq || `mock_upload_${crypto.randomUUID().replace(/-/g, "")}`;
    const urls = [];
    for (let i = 1; i <= partsCount; i++) {
      urls.push(
        `${process.env.APP_URL || "http://localhost:3000"}/api/v1/mock-r2-upload?key=${encodeURIComponent(objectKey)}&uploadId=${uploadId}&partNumber=${i}`
      );
    }
    return { uploadId, urls };
  }

  let uploadId = uploadIdFromReq;
  if (!uploadId) {
    const createCommand = new CreateMultipartUploadCommand({
      Bucket: getBucketName(),
      Key: objectKey,
    });
    const createResponse = await s3Client.send(createCommand);
    uploadId = createResponse.UploadId!;
  }

  const urls = [];
  for (let i = 1; i <= partsCount; i++) {
    const partCommand = new UploadPartCommand({
      Bucket: getBucketName(),
      Key: objectKey,
      UploadId: uploadId,
      PartNumber: i,
    });
    const url = await getSignedUrl(s3Client, partCommand, { expiresIn: 7200 });
    urls.push(url);
  }

  return { uploadId, urls };
}

export interface PartInfo {
  partNumber: number;
  etag: string;
  checksum?: string;
}

export async function completeMultipartUpload(
  objectKey: string,
  uploadId: string,
  parts: PartInfo[]
): Promise<{ etag?: string; checksumCrc64nvme?: string; error?: string }> {
  // B2 FIX: Use isMockStorage() instead of !s3Client to be consistent with
  // every other function in this file. When FORCE_MOCK_STORAGE=true but
  // s3Client is non-null, !s3Client would incorrectly try to use real S3.
  if (isMockStorage()) {
    const localPath = path.join(process.cwd(), "uploads_dev", objectKey);
    const dir = path.dirname(localPath);
    if (!fs.existsSync(dir)) {
      fs.mkdirSync(dir, { recursive: true });
    }

    try {
      const writeStream = fs.createWriteStream(localPath);
      
      const sortedParts = [...parts].sort((a, b) => a.partNumber - b.partNumber);

      for (const part of sortedParts) {
        const partPath = path.join(process.cwd(), "uploads_dev", `${objectKey}.part_${part.partNumber}`);
        if (!fs.existsSync(partPath)) {
          writeStream.end();
          return { error: `Part file ${part.partNumber} not found` };
        }
        const chunk = fs.readFileSync(partPath);
        writeStream.write(chunk);
        fs.unlinkSync(partPath);
      }
      await new Promise<void>((resolve, reject) => {
        writeStream.on("finish", () => resolve());
        writeStream.on("error", (err) => reject(err));
        writeStream.end();
      });

      const completedContent = fs.readFileSync(localPath);
      const checksum = crc64nvmeBase64(completedContent);
      
      fs.writeFileSync(
        localPath + ".meta",
        JSON.stringify({ sha256: "multipart_check_in_db", checksumCrc64nvme: checksum, size: completedContent.length })
      );

      return { etag: "mock_etag", checksumCrc64nvme: checksum };
    } catch (err: unknown) {
      const errMsg = err instanceof Error ? err.message : "Failed to write assembled mock file";
      return { error: errMsg };
    }
  }

  // B2 FIX: isMockStorage() covers both the null-client case and the
  // FORCE_MOCK_STORAGE flag. Guard the real path explicitly so TypeScript
  // can narrow s3Client to non-null.
  if (!s3Client) {
    return { error: "S3 client not initialized" };
  }

  try {
    const command = new CompleteMultipartUploadCommand({
      Bucket: getBucketName(),
      Key: objectKey,
      UploadId: uploadId,
      MultipartUpload: {
        Parts: parts.map(p => ({
          PartNumber: p.partNumber,
          ETag: p.etag,
        })),
      },
    });
    const response = await s3Client.send(command);
    return {
      etag: response.ETag,
      checksumCrc64nvme: response.ChecksumCRC64NVME,
    };
  } catch (error: unknown) {
    console.error("Failed to complete multipart upload in R2:", error);
    const errMsg = error instanceof Error ? error.message : "Complete multipart upload failed";
    return { error: errMsg };
  }
}

export const PREVIEW_TEXT_MAX_BYTES = 100 * 1024; // finding 7: previews read at most 100KB (+1 byte truncation probe)

// getObjectText reads a stored object as text with a hard memory cap: at
// most maxBytes+1 bytes are ever read from disk or requested from R2 (a
// Range header bounds the server response), so a 200MB share never gets
// buffered into memory by a preview request. The +1 byte detects truncation
// for callers that want to know the object was cut off.
export async function getObjectText(objectKey: string, maxBytes: number = PREVIEW_TEXT_MAX_BYTES): Promise<string> {
  if (!s3Client || process.env.FORCE_MOCK_STORAGE === "true") {
    const localPath = path.join(process.cwd(), "uploads_dev", objectKey);
    if (!fs.existsSync(localPath)) return "";
    let fd: number | null = null;
    try {
      fd = fs.openSync(localPath, "r");
      const buf = Buffer.alloc(maxBytes + 1);
      const bytesRead = fs.readSync(fd, buf, 0, maxBytes + 1, 0);
      return buf.subarray(0, bytesRead).toString("utf-8");
    } catch {
      return "";
    } finally {
      if (fd !== null) fs.closeSync(fd);
    }
  }

  try {
    const command = new GetObjectCommand({
      Bucket: getBucketName(),
      Key: objectKey,
      Range: `bytes=0-${maxBytes}`, // server-side truncation: body ≤ maxBytes+1 bytes
    });
    const response = await s3Client.send(command);
    if (response.Body) {
      return await response.Body.transformToString();
    }
  } catch (err) {
    console.error("Failed to fetch object content for preview:", err);
  }
  return "";
}

export async function calculateS3ObjectHash(objectKey: string): Promise<string> {
  if (!s3Client) throw new Error("S3 client not initialized");
  const command = new GetObjectCommand({
    Bucket: getBucketName(),
    Key: objectKey,
  });
  const response = await s3Client.send(command);
  const stream = response.Body as unknown as AsyncIterable<Buffer | Uint8Array>;
  if (!stream) throw new Error("Failed to get object body stream");

  const hash = crypto.createHash("sha256");
  for await (const chunk of stream) {
    hash.update(chunk);
  }
  return hash.digest("hex");
}
