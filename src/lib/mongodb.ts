import { MongoClient, Db } from "mongodb";
import { validateEnv } from "./env";

const uri = process.env.MONGODB_URI;

let client: MongoClient | null = null;
let clientPromise: Promise<MongoClient> | null = null;

function getClientPromise(): Promise<MongoClient> {
  if (clientPromise) return clientPromise;

  const mongoUri = uri || "mongodb://localhost:27017/r2-uplink";

  if (process.env.NODE_ENV === "development") {
    const globalWithMongo = global as typeof globalThis & {
      _mongoClientPromise?: Promise<MongoClient>;
    };

    if (!globalWithMongo._mongoClientPromise) {
      client = new MongoClient(mongoUri);
      globalWithMongo._mongoClientPromise = client.connect();
    }
    clientPromise = globalWithMongo._mongoClientPromise;
  } else {
    client = new MongoClient(mongoUri);
    clientPromise = client.connect();
  }

  return clientPromise;
}

export async function getDb(): Promise<Db> {
  validateEnv();
  const connection = await getClientPromise();
  return connection.db();
}

interface QuotaDocument {
  _id: string;
  storageBytes: number;
  reservedBytes: number;
  classAOps: number;
  classBOps: number;
  classAResetAt: Date;
  classBResetAt: Date;
  quotaEvents: Array<{ timestamp: Date; type: string; message: string }>;
}

let indexesPromise: Promise<void> | null = null;
// B12 FIX: Track last failure time to prevent rapid retry loops.
let lastIndexFailureAt = 0;
const INDEX_RETRY_COOLDOWN_MS = 30_000; // 30 seconds between retry attempts

// Helper to initialize indexes
export function initIndexes(): Promise<void> {
  if (indexesPromise) return indexesPromise;

  // B12 FIX: Enforce cooldown after failures to avoid hammering MongoDB
  // when indexes consistently fail (e.g., auth issues, network partitions).
  if (lastIndexFailureAt > 0 && Date.now() - lastIndexFailureAt < INDEX_RETRY_COOLDOWN_MS) {
    return Promise.resolve();
  }

  indexesPromise = (async () => {
    const db = await getDb();
    
    // Unique shareId index
    await db.collection("shares").createIndex({ shareId: 1 }, { unique: true });
    
    // Cleanup compound index
    await db.collection("shares").createIndex({ status: 1, expiresAt: 1 });
    
    // Integrity index for CAS
    await db.collection("shares").createIndex({ hashValue: 1 });

    // Unique sparse index for short downloadCode
    await db.collection("shares").createIndex({ downloadCode: 1 }, { unique: true, sparse: true });

    // NOTE: chat sessions live in Redis now (see src/lib/rooms.ts). The old
    // sessions/session_participants/session_files/session_messages
    // collections are no longer written; their indexes are intentionally
    // not created here.

    // Initialize quota tracking document if not present
    const quotaDoc = await db.collection<QuotaDocument>("quotas").findOne({ _id: "r2_quota" });
    if (!quotaDoc) {
      const now = new Date();
      const getNextMonthStart = (d: Date) => {
        const date = new Date(d);
        date.setUTCMonth(date.getUTCMonth() + 1);
        date.setUTCDate(1);
        date.setUTCHours(0, 0, 0, 0);
        return date;
      };
      const getNextDayStart = (d: Date) => {
        const date = new Date(d);
        date.setUTCDate(date.getUTCDate() + 1);
        date.setUTCHours(0, 0, 0, 0);
        return date;
      };
      await db.collection<QuotaDocument>("quotas").insertOne({
        _id: "r2_quota",
        storageBytes: 0,
        reservedBytes: 0,
        classAOps: 0,
        classBOps: 0,
        classAResetAt: getNextMonthStart(now),
        classBResetAt: getNextDayStart(now),
        quotaEvents: [
          {
            timestamp: now,
            type: "INFO",
            message: "Quota system initialized successfully.",
          },
        ],
      });
    }
    console.log("MongoDB Indexes and Quotas initialized successfully.");
  })();

  indexesPromise.catch((error) => {
    console.error("Failed to initialize MongoDB indexes:", error);
    lastIndexFailureAt = Date.now();
    indexesPromise = null;
  });

  return indexesPromise;
}
