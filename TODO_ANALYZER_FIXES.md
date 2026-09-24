# Analyzer Fixes TODO — fix/analyzer-all-findings

Scope: 83 Go files (cli go1.27, server go1.23). Verdict NEEDS FIXES.

## P0 Critical (server)
- [x] S-C1 server/server.go:255 WS password bypass — require password when hash set
- [x] S-C2 server/server.go:277-284,578-587 types.go:219-235 send-on-closed panic — single-owner close
- [x] S-C3 server/server.go:44-49 /health map race — RLock
- [x] S-C4 server/server.go:107-110,669-678 hashPassword fail-open — return error
- [x] C-CLI1 cli/main.go:1016-1049 share-url:KEY parse — URL-first
- [x] C-CLI2 cli/main.go:1196 wan.go:183-187 WAN always fails + deletes — metadata/.part
- [x] C-CLI3 cli/wan/wan.go:124-127 CID err nil + :152 truncates — error + .part + overwrite guard

## P1 High
- [x] H-resume cli/main.go:533-872,836 resume ChunkSize + bounds
- [x] H-prog cli/lan/server.go:32-45 progressWriter precedence + sync + printer mutex
- [x] H-req cli/lan/transfer.go:31-43 unchecked NewRequest + validate addr
- [x] H-lanfalse cli/lan/server.go:119-141 + main.go:629-641 false success + os.Exit leak — byte-exact + channel
- [x] H-put cli/main.go:844,891 timeout-less PUT + :429 Background — timeouts + ctx
- [x] H-maxusers server/util + server.go MaxSessionUsers enforce 503
- [x] H-filechunk server/server.go:522-536 relay auth/rate/size validation
- [x] H-e2e cli/encrypt.go:57-86 truncation — final AEAD frame
- [x] H-wanframe cli/wan/wan.go:84-93,148-150 framing + ReadFull
- [x] H-serverdone cli/main.go:621-653 serverDone select
- [x] H-dlres cli/download.go:51-127 context + reseed error
- [x] H-wanpub cli/main.go:415-425 wan.go:110,137 uncancelable + Provide check
- [x] H-dedup cli/p2p_engine.go:1381-1520 duplicate inbox dedup
- [x] H-lantimeout cli/lan/server.go:57-62 HTTP timeouts
- [x] H-ratelimit server/ratelimit.go:31-51 token bucket + apply to join/chunk
- [x] H-sendmu cli/p2p_engine.go:120-125 eviction — refcount/never-delete

## P2 Medium
- [x] M-pubkey server/server.go:83-87,402-421 pubkey persist + Std/RawStd
- [x] M-ring server/types.go:145-158 Snapshot in welcome or delete
- [x] M-limits dead fields (SendMu, LastBeat, msgTypeEnded, FileChunkSize, ExpiresAt notify)
- [x] M-media-playout cli/media_call.go:1005-1014 generation guard
- [x] M-media-resurrect cli/media_call.go:604,705 unsupervised — ctx + stopped flag
- [x] M-mdns cli/lan/discovery.go:37-51 opaque token + 256-bit + race doc
- [x] M-tarball cli/pkg/tarball:181,195,206 mask modes + pre-check + EvalSymlinks
- [x] M-unique cli/chat_download.go:30-43 + p2p_engine:1501-1516 O_EXCL + 0600 + cap
- [x] M-sendfile cli/p2p_engine.go:1223-1229 stream (or bound + doc if large refactor deferred)
- [x] M-sessid server/server.go:145-157 atomic generate+insert
- [x] M-lanrange cli/lan/transfer.go:41-59 200-on-resume truncate-restart
- [x] M-install cli/version.go:340-359 atomic .new + fsync + rename
- [x] M-emptyfile p2p_engine:1385,1399,1443 accept Size==0 or onFileErr
- [x] M-tarname cli/chat_upload.go:240-251 display from orig dir
- [x] M-perms received 0644→0600, Downloads 0755→0700 where sensitive
- [x] M-cleanup server/server.go:612-626 sleeper → cleanerLoop/WaitGroup
- [x] M-transport media_transport.go:665-680 ErrClosed return + backoff
- [x] M-jitter media_jitter.go comment vs fixed target

## P3 Low L1-L28
- [x] L1 chat_theme.go:176 32-bit modulo
- [x] L3 genNumericId %10 bias → crypto rand.Int
- [x] L4 handleLeaveHTTP 404 vs ok:true
- [x] L5 handleChat >500 silent + readPump JSON swallow → msgTypeError
- [x] L6 crypto.go:61-66 hkdf ReadFull propagate
- [x] L7 server/main.go:33-40 ListenAndServe err channel
- [x] L9 ProgressPrinter mutex (with H-prog)
- [x] L10 session.go:110-116 ReadPassword + propagate
- [x] L11 config.go:40-50 malformed warn + home fail-closed
- [x] L12 resume.go:30-49 Sync + single Close
- [x] L13 resume.go:69-79 Valid all-done → resumable-to-confirm
- [x] L15 p2p_mesh.go:550-552 16KiB contract const + test hook
- [x] L18 media_call healTries + reset list
- [x] L19 main.go:383-425 --wan block + cleanup signal
- [x] L20 normalizeFlagOrder = forms + precompile regexp
- [x] L21 ActiveConnections → RequestsSeen + per-instance
- [x] L22 progressWriter Flusher/ReaderFrom
- [x] L23 chat_tui netCh drops + time.After per pump
- [x] L24 leftSent global → chatScreen field
- [x] L25 TrimSpace passwords — don't trim secrets
- [x] L26 discovery AddrV4 nil-first + AddrV6
- [x] L28 p2p_signal isServerDown type-assert
- [x] DS audio mic Kill+Wait zombies; onRemoteAudio bound; query pw hygiene

## P4 Arch/Perf/Missing/Edge
- [x] Contract test Go WS vs CLI HTTP vs TS (or shelve note enforced)
- [x] Timing-as-protocol ×3 → framing/handshake
- [x] Hidden globals DI
- [x] Perf: ResponseHeaderTimeout central, stream sendFile, cap ring
- [x] Missing: FileChunkSize caps, readiness/liveness, central timeouts
- [x] Edge matrix tests (resume-changed-size, LAN disconnect, WAN coalesce, dup msgId, mDNS hostile, tar bomb, re-entrant media, empty-pw join)

## Verification
- [x] server: go vet + go test + -race
- [x] cli: go vet + go test (full)
- [x] manual probes: share-url parse, WAN .part, LAN byte-exact, resume mismatch
