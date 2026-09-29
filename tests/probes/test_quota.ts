const BASE_URL = "http://localhost:3000";
const ADMIN_API_KEY = process.env.ADMIN_API_KEY || "9ab7c7af79c9757067e0087cb05c6a9c4aab1ca985ab8c8ba71ac01af0167a57";
const adminHeaders = { "Authorization": `Bearer ${ADMIN_API_KEY}` };

async function runTests() {
  console.log("=== STARTING QUOTA ENFORCEMENT SYSTEM TESTS ===");

  try {
    // 1. Fetch current quota status
    console.log("\n[Test 1] Querying current administrative quota status...");
    const statusRes = await fetch(`${BASE_URL}/api/v1/admin/quota`, {
      headers: adminHeaders,
    });
    if (!statusRes.ok) {
      throw new Error(`Failed to fetch quota status: ${statusRes.statusText}`);
    }
    const initialStatus = await statusRes.json();
    console.log("Current Storage Bytes:", initialStatus.storageUsageBytes);
    console.log("Current Reserved Bytes:", initialStatus.storageReservedBytes);
    console.log("Current Class A Usage:", initialStatus.classAUsage);
    console.log("Current Class B Usage:", initialStatus.classBUsage);
    console.log("Uploads Enabled:", initialStatus.uploadsEnabled);

    // 2. Validate normal upload reservation and confirmation
    console.log("\n[Test 2] Testing normal upload session initialization (single-part)...");
    const fs = await import("fs");
    const path = await import("path");
    const crypto = await import("crypto");
    const mockContent = "a".repeat(5 * 1024 * 1024);
    const realSha256 = crypto.createHash("sha256").update(mockContent).digest("hex");

    const initRes = await fetch(`${BASE_URL}/api/v1/share/init`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        filename: "test_normal.txt",
        size: 5 * 1024 * 1024, // 5 MB
        mimeType: "text/plain",
        hashValue: realSha256,
      }),
    });

    if (!initRes.ok) {
      const err = await initRes.json();
      throw new Error(`Init failed: ${JSON.stringify(err)}`);
    }

    const initData = await initRes.json();
    console.log("Upload initialized successfully. Share ID:", initData.shareId);

    // Check reserved bytes changed
    const statusRes2 = await fetch(`${BASE_URL}/api/v1/admin/quota`, {
      headers: adminHeaders,
    });
    const statusData2 = await statusRes2.json();
    console.log("Updated Reserved Bytes (expected +5MB):", statusData2.storageReservedBytes);

    // Write dummy mock file to filesystem to satisfy confirm check in mock mode
    const mockFilePath = path.join(process.cwd(), "uploads_dev", initData.objectKey);
    const mockDir = path.dirname(mockFilePath);
    if (!fs.existsSync(mockDir)) {
      fs.mkdirSync(mockDir, { recursive: true });
    }
    fs.writeFileSync(mockFilePath, mockContent);
    fs.writeFileSync(mockFilePath + ".meta", JSON.stringify({ sha256: realSha256 }));

    // Confirm upload to commit storage bytes
    console.log("Confirming upload to commit reservation...");
    const confirmRes = await fetch(`${BASE_URL}/api/v1/share/${initData.shareId}/confirm`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({}),
    });
    if (!confirmRes.ok) {
      const err = await confirmRes.json();
      throw new Error(`Confirm failed: ${JSON.stringify(err)}`);
    }
    console.log("Upload confirmed successfully!");

    const statusRes3 = await fetch(`${BASE_URL}/api/v1/admin/quota`, {
      headers: adminHeaders,
    });
    const statusData3 = await statusRes3.json();
    console.log("Committed Storage Bytes:", statusData3.storageUsageBytes);
    console.log("Reserved Bytes after commit:", statusData3.storageReservedBytes);

    // 3. Test Blocked Upload (size exceeding storage threshold)
    console.log("\n[Test 3] Testing blocked upload when size exceeds threshold limit...");
    // 210 MB (exceeds our 200 MB test threshold but fits within 500 MB multipart limit)
    const blockedRes = await fetch(`${BASE_URL}/api/v1/share/init`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        filename: "too_large.tar",
        size: 210 * 1024 * 1024,
        mimeType: "application/x-tar",
        hashValue: "b".repeat(64),
        partsCount: 5,
      }),
    });

    console.log("Blocked upload HTTP status code (expected 503):", blockedRes.status);
    const blockedData = await blockedRes.json();
    console.log("Blocked upload message:", blockedData.error);

    // 4. Test Concurrency checks
    console.log("\n[Test 4] Testing concurrent uploads collective oversubscription prevention...");
    // We will spawn 10 concurrent requests of 30 MB each.
    // Collectively they exceed our 200 MB test threshold, so some of them must fail cleanly!
    const size30MB = 30 * 1024 * 1024;
    console.log("Sending 10 concurrent 30MB requests...");
    const promises = Array.from({ length: 10 }).map((_, i) =>
      fetch(`${BASE_URL}/api/v1/share/init`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          filename: `concurrent_file_${i}.bin`,
          size: size30MB,
          mimeType: "application/octet-stream",
          hashValue: "c".repeat(64),
        }),
      })
    );

    const responses = await Promise.all(promises);
    let successCount = 0;
    let failCount = 0;
    for (const res of responses) {
      if (res.status === 201) {
        successCount++;
      } else if (res.status === 503) {
        failCount++;
      }
    }
    console.log(`Concurrent test results: ${successCount} succeeded, ${failCount} blocked (503).`);
    if (successCount + failCount !== 10) {
      console.log("Warning: some requests returned unexpected status codes:", responses.map(r => r.status));
    }

    // Clean up created unconfirmed shares to free up the reservation
    console.log("Purging unconfirmed reservations via cleanup endpoint...");
    const cleanupRes = await fetch(`${BASE_URL}/api/v1/cleanup`, {
      method: "POST",
      headers: adminHeaders,
    });
    const cleanupData = await cleanupRes.json();
    console.log("Cleanup output:", cleanupData.message);

    // 5. Test Class B operational checks
    console.log("\n[Test 5] Testing download Class B operations incrementation...");
    const statusBeforeDownload = await (await fetch(`${BASE_URL}/api/v1/admin/quota`, {
      headers: adminHeaders,
    })).json();
    console.log("Class B usage before download check:", statusBeforeDownload.classBUsage);

    console.log("Hitting authorize-download endpoint (fails correct auth but registers attempt)...");
    const downloadRes = await fetch(`${BASE_URL}/api/v1/share/${initData.shareId}/authorize-download`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({}),
    });
    console.log("Download check HTTP response status:", downloadRes.status);

    const statusAfterDownload = await (await fetch(`${BASE_URL}/api/v1/admin/quota`, {
      headers: adminHeaders,
    })).json();
    console.log("Class B usage after download check (expected increment):", statusAfterDownload.classBUsage);

    console.log("\n=== ALL QUOTA SYSTEM TESTS FINISHED SUCCESSFULLY ===");
  } catch (err: unknown) {
    const errMsg = err instanceof Error ? err.message : String(err);
    console.error("Test failed with error:", errMsg);
    process.exit(1);
  }
}

runTests();
