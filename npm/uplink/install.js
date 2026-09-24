"use strict";
// Postinstall: fetch the matching uplink binary from GitHub Releases,
// verify it against checksums.txt, and place it at bin/uplink.
// Zero dependencies: https + crypto + system tar/unzip only.
const { execFileSync } = require("child_process");
const crypto = require("crypto");
const fs = require("fs");
const https = require("https");
const os = require("os");
const path = require("path");

const REPO = "AdityaAgrawal08/uplink";
const ASSETS = {
  "linux-x64": "uplink-linux-amd64.tar.gz",
  "linux-arm64": "uplink-linux-arm64.tar.gz",
  "darwin-x64": "uplink-darwin-amd64.tar.gz",
  "darwin-arm64": "uplink-darwin-arm64.tar.gz",
  "win32-x64": "uplink-windows-amd64.zip",
  "win32-arm64": "uplink-windows-arm64.zip",
};

function fail(msg) {
  console.error(`@aditya/uplink postinstall: ${msg}`);
  process.exit(1);
}

function get(url, binary) {
  return new Promise((resolve, reject) => {
    const headers = { "User-Agent": "@aditya/uplink-installer" };
    if (process.env.GITHUB_TOKEN) headers.Authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
    https
      .get(url, { headers }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          // Releases redirect to object storage; the token must NOT leak there.
          return get(res.headers.location, binary).then(resolve, reject);
        }
        if (res.statusCode === 404 && !process.env.GITHUB_TOKEN) {
          reject(new Error("asset not found (404) for this version — it may not be released yet."));
          res.resume();
          return;
        }
        if (res.statusCode !== 200) {
          reject(new Error(`download returned status ${res.statusCode}`));
          res.resume();
          return;
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(binary ? Buffer.concat(chunks) : Buffer.concat(chunks).toString("utf8")));
        res.on("error", reject);
      })
      .on("error", reject);
  });
}

async function main() {
  const key = `${process.platform}-${process.arch}`;
  const asset = ASSETS[key];
  if (!asset) fail(`unsupported platform ${key} (supported: ${Object.keys(ASSETS).join(", ")})`);
  const { version } = require("./package.json");
  const tag = `v${version}`;
  const base = `https://github.com/${REPO}/releases/download/${tag}`;

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uplink-dl-"));
  const cleanup = () => fs.rmSync(tmp, { recursive: true, force: true });
  try {
    const sums = await get(`${base}/checksums.txt`);
    const line = sums.split("\n").find((l) => l.trim().endsWith(` ${asset}`));
    if (!line) fail(`no checksum entry for ${asset} in ${tag}`);
    const expected = line.trim().split(/\s+/)[0];

    const data = await get(`${base}/${asset}`, true);
    const actual = crypto.createHash("sha256").update(data).digest("hex");
    if (actual.toLowerCase() !== expected.toLowerCase()) {
      fail("checksum mismatch — refusing to install (do not run this binary)");
    }

    const archive = path.join(tmp, asset);
    fs.writeFileSync(archive, data);
    const outDir = path.join(tmp, "out");
    fs.mkdirSync(outDir);
    if (asset.endsWith(".zip")) {
      if (process.platform !== "win32") fail(`${asset} is Windows-only; wrong asset resolved`);
      execFileSync("powershell", ["-NoProfile", "-Command", `Expand-Archive -Path '${archive}' -DestinationPath '${outDir}' -Force`], { stdio: "inherit" });
    } else {
      execFileSync("tar", ["-xzf", archive, "-C", outDir], { stdio: "inherit" });
    }
    const found = [];
    (function walk(d) {
      for (const e of fs.readdirSync(d, { withFileTypes: true })) {
        const p = path.join(d, e.name);
        if (e.isDirectory()) walk(p);
        else if (/^uplink(\.exe)?$/.test(e.name)) found.push(p);
      }
    })(outDir);
    if (found.length === 0) fail("archive contained no uplink binary");
    const binDir = path.join(__dirname, "bin");
    fs.mkdirSync(binDir, { recursive: true });
    const dest = path.join(binDir, process.platform === "win32" ? "uplink.exe" : "uplink");
    fs.copyFileSync(found[0], dest);
    if (process.platform !== "win32") fs.chmodSync(dest, 0o755);
    console.log(`@aditya/uplink: installed ${asset} (${tag})`);
  } catch (err) {
    fail(err.message);
  } finally {
    cleanup();
  }
}

if (require.main === module) main();
