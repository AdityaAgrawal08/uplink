import fs from "fs";
import path from "path";
import { HeadObjectCommand, S3Client } from "@aws-sdk/client-s3";
import { MongoClient } from "mongodb";

function loadEnv() {
    const raw = fs.readFileSync(path.join(process.cwd(), ".env"), "utf-8");
    for (const line of raw.split("\n")) {
        const m = line.match(/^\s*([A-Z0-9_]+)\s*=\s*(.*)\s*$/);
        if (m && !process.env[m[1]]) process.env[m[1]] = m[2].replace(/^"|"$/g, "");
    }
}

async function main() {
    loadEnv();

    const mongo = new MongoClient(process.env.MONGODB_URI!);
    await mongo.connect();
    const db = mongo.db();
    const share = await db.collection("shares").findOne(
        {},
        { sort: { createdAt: -1 }, projection: { shareId: 1, status: 1, size: 1, objectKey: 1, checksumCrc64nvme: 1 } }
    );
    console.log("share:", JSON.stringify(share, null, 2));
    await mongo.close();

    if (!share?.objectKey) return;

    const s3 = new S3Client({
        region: "auto",
        endpoint: process.env.R2_ENDPOINT_URL,
        credentials: {
            accessKeyId: process.env.R2_ACCESS_KEY_ID!,
            secretAccessKey: process.env.R2_SECRET_ACCESS_KEY!,
        },
    });
    const head = await s3.send(new HeadObjectCommand({ Bucket: process.env.R2_BUCKET_NAME!, Key: share.objectKey }));
    console.log("HEAD:", JSON.stringify({
        contentLength: head.ContentLength,
        checksumCRC32: head.ChecksumCRC32,
        checksumCRC32C: head.ChecksumCRC32C,
        checksumCRC64NVME: head.ChecksumCRC64NVME,
        checksumSHA1: head.ChecksumSHA1,
        checksumSHA256: head.ChecksumSHA256,
        checksumType: head.ChecksumType,
        etag: head.ETag,
    }, null, 2));
}

main().catch(e => { console.error(e); process.exit(1); });
