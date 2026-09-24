#!/usr/bin/env node
"use strict";
// Launcher shim (ships inside the tarball, so `npm i -g` registers the
// `uplink` command). Execs the platform binary fetched by postinstall,
// which lives next to this file as uplink / uplink.exe.
const { spawnSync } = require("child_process");
const fs = require("fs");
const path = require("path");

const exe = path.join(__dirname, process.platform === "win32" ? "uplink.exe" : "uplink");
if (!fs.existsSync(exe)) {
  console.error("@aditya/uplink: binary missing — reinstall with: npm install -g @aditya/uplink");
  process.exit(1);
}
const r = spawnSync(exe, process.argv.slice(2), { stdio: "inherit" });
process.exit(r.status === null ? 1 : r.status);
