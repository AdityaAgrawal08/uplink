import type { NextConfig } from "next";

// B29 FIX: The previous CSP (`default-src 'self'; script-src 'self';
// style-src 'self';`) broke core functionality:
//   - QR codes are rendered from `data:` URLs → need img-src data:
//   - image/video/audio previews and PDF iframes load from R2 presigned
//     URLs (cross-origin) → need img-src/media-src/frame-src https:
//   - React inline style attributes and highlight.js styles need
//     style-src 'unsafe-inline'
// Each directive below is scoped to what the app actually uses.
const csp = [
  "default-src 'self'",
  // Next.js hydration/flight inline scripts require 'unsafe-inline'.
  // (Switch to a nonce-based policy if stricter script control is needed.)
  "script-src 'self' 'unsafe-inline'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data: blob: https:",
  "media-src 'self' blob: https:",
  "frame-src 'self' https:",
  "connect-src 'self' https:",
  "font-src 'self' data:",
  "object-src 'none'",
  "base-uri 'self'",
  "form-action 'self'",
].join("; ");

const nextConfig: NextConfig = {
  async headers() {
    return [
      {
        source: "/api/(.*)",
        headers: [
          { key: "Cache-Control", value: "private, no-store, no-cache, must-revalidate" },
          { key: "CDN-Cache-Control", value: "no-store" },
          { key: "Vercel-CDN-Cache-Control", value: "no-store" },
        ],
      },
      {
        source: "/(.*)",
        headers: [
          { key: "Content-Security-Policy", value: csp },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Strict-Transport-Security", value: "max-age=31536000; includeSubDomains" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
        ],
      },
    ];
  },
};

export default nextConfig;
