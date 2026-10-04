import { getDb } from "@/lib/mongodb";
import FilePreview from "@/components/FilePreview";
import { notFound } from "next/navigation";

// B14 FIX: This page reads live share state (status/expiry) from MongoDB.
// Without an explicit directive the Next.js full-route cache could serve a
// stale shell for a share that has since expired or been revoked. Force
// per-request rendering.
export const dynamic = "force-dynamic";

export default async function SharePage(props: { params: Promise<{ id: string }> }) {
  const { id } = await props.params;
  const db = await getDb();
  
  const share = await db.collection("shares").findOne({
    $or: [{ shareId: id }, { downloadCode: id }],
    status: "ACTIVE",
  });

  // B14 FIX: honor expiresAt. The API routes reject expired shares, but this
  // page only filtered on status — a share past its expiry but not yet swept
  // would still render a download UI.
  if (!share || (share.expiresAt && new Date(share.expiresAt) < new Date())) {
    notFound();
  }

  const serverDownloadUrl = "";

  const shareMeta = {
    shareId: share.shareId,
    filename: share.filename,
    size: share.size,
    mimeType: share.mimeType,
    hashValue: share.hashValue,
    passwordRequired: !!share.passwordHash,
    isEncrypted: !!share.isEncrypted,
    createdAt: share.createdAt ? new Date(share.createdAt).toISOString() : null,
    expiresAt: share.expiresAt ? new Date(share.expiresAt).toISOString() : null,
    downloadUrl: serverDownloadUrl,
  };

  return (
    <div className="container">
      <h1>R2-Uplink File Share</h1>
      <p>This resource is available for download and inline web preview.</p>
      
      <h2>Download Command</h2>
      <pre className="code-block">
        uplink receive {share.shareId}
      </pre>

      <h2>First Time? Install the CLI</h2>
      <pre className="code-block">
        curl -sSf https://raw.githubusercontent.com/AdityaAgrawal08/uplink/main/install.sh | sh
      </pre>

      <FilePreview share={shareMeta} />
    </div>
  );
}
