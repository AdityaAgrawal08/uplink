# @aditya/uplink

E2E-encrypted terminal chat and file sharing (WebRTC P2P + serverless relay).

```sh
npm install -g @aditya/uplink

uplink          # open CREATE / JOIN screen
uplink send report.pdf
uplink receive <code-or-link> [dest]
```

Update with `npm update -g @aditya/uplink` (or `uplink update`).

## How it works

The npm tarball ships a tiny launcher. On install, a postinstall script
fetches the matching `uplink` binary from GitHub Releases, verifies it
against the release `checksums.txt`, and places it beside the launcher.
No build step, no dependencies.

If the install fails with an asset 404, that version's binaries are not
released yet — retry after the release workflow finishes.
