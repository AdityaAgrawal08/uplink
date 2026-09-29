# Tests

All web/E2E test suites and manual probes live here. Anything that is **not**
a Go `_test.go` file compiled against a package is under `tests/`; the Go
tests stay colocated with their packages (see "Why the Go tests stay put"
below).

## Directory layout

```text
tests/
├── web/       unit suites for the Next.js app
│   ├── run_tests.ts        node:test runner (crypto, crc64, redis-mock parity)
│   ├── crypto.test.ts      vitest — filename-sanitization parity with the Go CLI
│   ├── redis.test.ts       vitest — MockRedis semantics + production gating
│   └── rooms.test.ts       vitest — rooms signaling plane (CRUD, roles, inbox)
├── e2e/       end-to-end scripts against a live server + built CLI
│   ├── e2e_phase0.sh       transfer matrix: send/receive, folders, E2EE, multipart,
│   │                       idempotency, negative contracts
│   ├── session_flow_test.sh  signaling lifecycle: create/join/heartbeat/signal/
│   │                       inbox/guards/leave (skips if session routes are absent)
│   └── chat_two_clients.sh   two-client P2P chat: cross-delivery, roster, end
│                           propagation, room-destroyed-on-last-leave
└── probes/    manual debug probes — NOT run by CI, not part of `make test`
    ├── probe_r2.ts         inspect latest Mongo share + S3/R2 object checksums
    └── test_quota.ts       quota enforcement walkthrough against a live server
```

## Which suite runs where

| Suite | Runner | Entry point | Needs |
| --- | --- | --- | --- |
| Web unit (tsx) | node:test via tsx | `tests/web/run_tests.ts` | nothing (in-memory) |
| Web unit (vitest) | vitest | `tests/web/*.test.ts` (3 files) | nothing (mock Redis) |
| Go unit | `go test` | in-package `_test.go` files | nothing (plus `-race`) |
| E2E | bash/curl/CLI | `tests/e2e/*.sh` | live server on `$SERVER`, built CLI |
| Probes | tsx (manual) | `tests/probes/*.ts` | `.env` + live server (see below) |

CI runs the Go suites, the web unit suites, the three e2e scripts, and the
API contract probes — but **not** `tests/probes/`.

## Commands (from the repo root)

```sh
make test       # everything that needs no external services:
                # test-go + test-web
make test-go    # cd cli && go test -race ./... ; cd server && go test -race ./...
make test-web   # npx tsx tests/web/run_tests.ts ; npx vitest run
make e2e        # the three tests/e2e/*.sh against SERVER (default
                # http://localhost:3000); needs cli/build/uplink first
make test-all   # test, then e2e
```

`make e2e` requires a running server and a CLI build:

```sh
make build                  # produces cli/build/uplink
npm run build && npm start  # production server on :3000
make e2e                    # SERVER=http://localhost:3000 exported by default
# override: make e2e SERVER=http://127.0.0.1:3000
```

Individual scripts work the same way:

```sh
SERVER=http://localhost:3000 ./tests/e2e/e2e_phase0.sh
SERVER=http://localhost:3000 ./tests/e2e/session_flow_test.sh
SERVER=http://localhost:3000 ./tests/e2e/chat_two_clients.sh
```

`session_flow_test.sh` exits 0 with a skip notice when the session routes are
not present on the branch. `e2e_phase0.sh` has an optional slow path:
`RUN_SLOW=1` enables the ~80s stale-idempotency-cache scenario.

## Manual probes (`tests/probes/`)

Not run by CI. `probe_r2.ts` reads `.env` for Mongo/R2 credentials and queries
the latest share plus its object checksums — needs a populated database.
`test_quota.ts` drives the quota endpoints against a live server via
`ADMIN_API_KEY`. Run them from the repo root with tsx:

```sh
npx tsx tests/probes/probe_r2.ts
ADMIN_API_KEY=... npx tsx tests/probes/test_quota.ts
```

## Why the Go tests stay put

The Go toolchain only discovers `_test.go` files in the **same directory as
the package they compile against** (`go test ./...` walks directories, not
across arbitrary paths), and these tests are in-package: they exercise
unexported identifiers of `cli/` and `server/`. Moving them out would break
discovery and compilation alike. The web layer has no such constraint, which
is why its suites live here under `tests/`. Please don't move the Go tests.