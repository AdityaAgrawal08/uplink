"use client";

import { useState, useEffect, useCallback, useRef } from "react";
import QRCode from "qrcode";
import SyntaxHighlighter from "./SyntaxHighlighter";

interface ShareMetadata {
  shareId: string;
  filename: string;
  size: number;
  mimeType: string;
  hashValue: string;
  passwordRequired: boolean;
  isEncrypted?: boolean;
  createdAt: string | null;
  expiresAt: string | null;
  downloadUrl?: string;
}

interface Props {
  share: ShareMetadata;
}

type PreviewType = "text" | "image" | "video" | "audio" | "pdf" | "other";

function getPreviewType(mime: string, name: string): PreviewType {
  const m = mime.toLowerCase();
  if (m.startsWith("text/")) return "text";
  if (m.startsWith("image/")) return "image";
  if (m.startsWith("video/")) return "video";
  if (m.startsWith("audio/")) return "audio";
  if (m === "application/pdf") return "pdf";

  const ext = name.split(".").pop()?.toLowerCase() || "";
  if (["json", "js", "ts", "py", "go", "rs", "md", "html", "css", "yaml", "xml", "ini", "conf", "sh", "bat"].includes(ext)) {
    return "text";
  }
  return "other";
}

export default function FilePreview({ share }: Props) {
  const [password, setPassword] = useState("");
  const [authorized, setAuthorized] = useState(!share.passwordRequired);
  const [downloadUrl, setDownloadUrl] = useState(share.downloadUrl || "");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const [textContent, setTextContent] = useState<string | null>(null);
  const [textLoading, setTextLoading] = useState(false);

  const [qrUrl, setQrUrl] = useState("");
  const [copiedLink, setCopiedLink] = useState(false);
  const [copiedText, setCopiedText] = useState(false);

  // B15 FIX: Use a ref to track the download URL to avoid the useCallback
  // dependency cycle. The old code included `downloadUrl` (a state variable)
  // in the useCallback deps, causing unnecessary re-creation on every
  // state change and triggering the useEffect in a loop.
  const downloadUrlRef = useRef(downloadUrl);
  downloadUrlRef.current = downloadUrl;

  const previewType = getPreviewType(share.mimeType, share.filename);
  const fileExt = share.filename.split(".").pop()?.toLowerCase() || "txt";

  useEffect(() => {
    if (typeof window !== "undefined") {
      QRCode.toDataURL(window.location.href, {
        margin: 2,
        errorCorrectionLevel: "Q", // Q offers high reliability with reasonable density
        width: 256,
      })
        .then((url) => setQrUrl(url))
        .catch((err) => console.error("Failed to generate QR code:", err));
    }
  }, []);

  // B15 FIX: Use downloadUrlRef.current instead of downloadUrl in the
  // dependency array to avoid unnecessary re-creation when the state changes.
  const ensureDownloadUrl = useCallback(async () => {
    if (downloadUrlRef.current) return downloadUrlRef.current;
    try {
      const res = await fetch(`/api/v1/share/${share.shareId}/authorize-download`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      });
      const data = await res.json();
      if (res.ok) {
        setDownloadUrl(data.downloadUrl);
        return data.downloadUrl;
      }
    } catch {}
    return "";
  }, [share.shareId, password]);

  useEffect(() => {
    if (authorized && !downloadUrl && previewType !== "other" && previewType !== "text") {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      ensureDownloadUrl();
    }
  }, [authorized, downloadUrl, previewType, ensureDownloadUrl]);

  const formatDate = (isoString: string | null) => {
    if (!isoString) return "N/A";
    const date = new Date(isoString);
    return date.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  };

  const formatBytes = (bytes: number) => {
    if (bytes === 0) return "0 B";
    const k = 1024;
    const sizes = ["B", "KB", "MB", "GB"];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
  };

  const handleAuthorize = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError("");

    try {
      const res = await fetch(`/api/v1/share/${share.shareId}/authorize-download`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      });

      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || "Failed to authorize");
      }

      setDownloadUrl(data.downloadUrl);
      setAuthorized(true);
    } catch (err: unknown) {
      const errMsg = err instanceof Error ? err.message : String(err);
      setError(errMsg || "Invalid password");
    } finally {
      setLoading(false);
    }
  };

  const fetchTextContent = async () => {
    setTextLoading(true);
    setError("");
    try {
      const res = await fetch(`/api/v1/share/${share.shareId}/preview-text`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password }),
      });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || "Failed to load text preview");
      }
      setTextContent(data.text);
    } catch (err: unknown) {
      const errMsg = err instanceof Error ? err.message : String(err);
      setError(errMsg || "Failed to load text preview");
    } finally {
      setTextLoading(false);
    }
  };



  const handleDownload = () => {
    if (downloadUrl) {
      const a = document.createElement("a");
      a.href = downloadUrl;
      a.download = share.filename;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      return;
    }

    setLoading(true);
    ensureDownloadUrl().then((url) => {
      setLoading(false);
      if (url) {
        const a = document.createElement("a");
        a.href = url;
        a.download = share.filename;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
      } else {
        setError("Failed to retrieve file download link.");
      }
    });
  };

  const handleShare = async () => {
    if (typeof navigator !== "undefined" && navigator.share) {
      try {
        await navigator.share({
          title: `Shared File: ${share.filename}`,
          text: `Download ${share.filename} from Uplink.`,
          url: window.location.href,
        });
        return;
      } catch {}
    }
    handleCopyLink();
  };

  const handleCopyLink = () => {
    if (typeof navigator !== "undefined") {
      navigator.clipboard.writeText(window.location.href);
      setCopiedLink(true);
      setTimeout(() => setCopiedLink(false), 2000);
    }
  };

  const handleCopyText = () => {
    if (typeof navigator !== "undefined" && textContent) {
      navigator.clipboard.writeText(textContent);
      setCopiedText(true);
      setTimeout(() => setCopiedText(false), 2000);
    }
  };

  const handleDownloadTextLocal = () => {
    if (!textContent) return;
    const blob = new Blob([textContent], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = share.filename.endsWith(".txt") ? share.filename : `${share.filename}.txt`;
    link.click();
    URL.revokeObjectURL(url);
  };

  if (share.isEncrypted) {
    return (
      <div className="preview-container password-box">
        <h3>🔒 End-to-End Encrypted</h3>
        <p>This file is encrypted client-side and cannot be decrypted by the browser.</p>
        <p className="subtitle">Please use the Uplink CLI to download and decrypt this file.</p>
      </div>
    );
  }

  if (!authorized) {
    return (
      <div className="preview-container password-box">
        <h3>🔑 Password Protected</h3>
        <p className="subtitle">Enter password to preview and download file</p>
        <form onSubmit={handleAuthorize} className="flex-row">
          <input
            type="password"
            placeholder="Password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            className="input-field"
          />
          <button type="submit" disabled={loading} className="btn-primary">
            {loading ? "Verifying..." : "Unlock"}
          </button>
        </form>
        {error && <p className="error-text">{error}</p>}
      </div>
    );
  }



  return (
    <div className="share-card-layout">
      {/* Meta Header Panel */}
      <div className="meta-header-box">
        <div className="meta-text-group">
          <h2>{share.filename}</h2>
          <div className="meta-info-grid">
            <div className="meta-item">
              <span className="meta-label">Size</span>
              <span className="meta-val">{formatBytes(share.size)}</span>
            </div>
            <div className="meta-item">
              <span className="meta-label">Uploaded</span>
              <span className="meta-val">{formatDate(share.createdAt)}</span>
            </div>
            <div className="meta-item">
              <span className="meta-label">Expires</span>
              <span className="meta-val expiry-val">{formatDate(share.expiresAt)}</span>
            </div>
          </div>
        </div>

        {/* Compact QR Code Container */}
        {qrUrl && (
          <div className="qr-container-wrapper">
            <div className="qr-container">
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img src={qrUrl} alt="Scan to share" width={64} height={64} className="qr-image" />
            </div>
            <span className="qr-caption">Scan to Share</span>
          </div>
        )}
      </div>

      {/* Action Toolbar */}
      <div className="action-toolbar">
        <button onClick={handleDownload} disabled={loading} className="btn-primary flex-center">
          {loading ? "Preparing..." : "📥 Download Original"}
        </button>
        <button onClick={handleShare} className="btn-secondary flex-center">
          {copiedLink ? "✓ Link Copied" : "🔗 Share Link"}
        </button>
      </div>

      {/* Preview Section */}
      <div className="preview-container">
        <h3>📄 File Preview</h3>
        <div className="preview-body">
          {previewType === "text" && (
            <div className="text-preview-container">
              {!textContent ? (
                <div className="text-preview-teaser">
                  <p className="subtitle">Text preview is available for this file.</p>
                  <button onClick={fetchTextContent} disabled={textLoading} className="btn-secondary">
                    {textLoading ? "Loading preview..." : "🔍 Show Text Preview"}
                  </button>
                </div>
              ) : (
                <div className="text-preview-box-enhanced">
                  <div className="text-action-header">
                    <span className="text-badge">{fileExt.toUpperCase()}</span>
                    <div className="flex-row-small">
                      <button onClick={handleCopyText} className="btn-text">
                        {copiedText ? "✓ Copied" : "📋 Copy"}
                      </button>
                      <button onClick={handleDownloadTextLocal} className="btn-text">
                        💾 Save as .txt
                      </button>
                    </div>
                  </div>
                  <div className="syntax-scroller">
                    <SyntaxHighlighter code={textContent} language={fileExt} />
                  </div>
                  {share.size > 100000 && (
                    <p className="preview-note">* Content truncated to first 100 KB</p>
                  )}
                </div>
              )}
            </div>
          )}

          {previewType === "image" && downloadUrl && (
            <div className="media-preview-box">
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img src={downloadUrl} alt={share.filename} loading="lazy" className="img-preview" />
            </div>
          )}

          {previewType === "video" && downloadUrl && (
            <div className="media-preview-box">
              <video src={downloadUrl} controls preload="metadata" className="video-preview" />
            </div>
          )}

          {previewType === "audio" && downloadUrl && (
            <div className="media-preview-box">
              <audio src={downloadUrl} controls className="audio-preview" />
            </div>
          )}

          {previewType === "pdf" && downloadUrl && (
            <div className="pdf-preview-box">
              <iframe src={downloadUrl} className="pdf-frame" title="PDF Preview" />
            </div>
          )}

          {previewType === "other" && (
            <div className="no-preview-box">
              <p className="no-preview-title">⚠️ Preview is not supported for this file format.</p>
              <p className="subtitle">You can still download the original file safely using the button above.</p>
            </div>
          )}

          {error && <p className="error-text">{error}</p>}
        </div>
      </div>
    </div>
  );
}
