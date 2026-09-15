# uplink-chat

E2E-encrypted terminal chat (WebRTC P2P + serverless relay). This package
installs the `uplink` binary for your platform on `npm install`.

```bash
npm install -g uplink-chat
uplink          # or: uplink-chat
```

Update with `npm update -g uplink-chat` (or `uplink update`).

## Private releases

If the GitHub repo is private, downloads need a token:

```bash
GITHUB_TOKEN=<token> npm install -g uplink-chat
```

## For maintainers

- Bump `version` here on every app release (tag `vX.Y.Z` must exist with
  matching assets + `checksums.txt`).
- Publish: `npm publish` (needs `NPM_TOKEN`; CI does this on release tags).
