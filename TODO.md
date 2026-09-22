# Media audit remediation — live progress

Legend: [ ] todo · [~] in progress · [x] done

## 1. C1 — Late-joiner scope admission (critical)
- [ ] PublishTo admits live members to videoTo/audioTo per kind on
- [ ] Regression: 3-party signal-path test, first announce dropped
- [ ] Full suite green after change

## 2. C2 — FEC streak cap (critical, the hum)
- [ ] Cap consecutive FEC ticks per peer (~8), then silence; reset on real packet
- [ ] Test: synthetic gap stream → silence after cap, no hum energy

## 3. H3 — Mixer normalize-on-clip (loudness)
- [ ] Replace always-on soft-clip with passthrough + normalize-only-on-clip
- [ ] Test: single talker passthrough; two talkers no-clip

## 4. H1 — Prune grace (flap-proof streams)
- [ ] Absent-grace: prune only after ~3 consecutive absent ticks
- [ ] Test: flapping roster keeps stream alive

## 5. H2 — Stop-notes on local start failure
- [ ] sendStop to announced scope when mic/camera start fails post-announce
- [ ] Test: failed start withdraws watchers

## 6. H4+M2 — Async health I/O + TX generation guard
- [ ] go joinPeer in healthCheck; renotify send outside lock in goroutine
- [ ] camera generation counter kills stale TX loops
- [ ] Rapid-toggle race test

## 7. M1+M5 — Speaker-death emit + scope-shift line
- [ ] writer death emits one diagnostic line
- [ ] PublishTo emits line when scope shifts under active publish

## 8. Verification
- [x] Full CLI suite green
- [x] -race media set green (incl. FEC/mixer/prune/withdraw/toggle/speaker tests)
- [x] tsc + eslint + vitest green
- [x] 6-target CGO-off builds green
- [x] Commit + push + install binary (4e01a7a)
- [x] CI fix verified with ffmpeg/ffplay/paplay absent: late-joiner
  passes under -race, mic e2e skips instead of hanging

## 9. Commands branch follow-ups (roles / moderation / palette)
- [x] Server roles (creator/admin/member, banned set) + /kick + /admin routes + tests
- [x] CLI /kick /admin /unadmin + fake-server endpoints + tests; committed on `commands`
- [ ] TestPaletteNavigationAndTabCompletes green (repro verbose line, fix ranking/selection)
- [ ] Palette scroll window: arrows reach all 8 commands, 6-row budget unchanged
- [ ] Full Go + TS suites green, commit on `commands`

## 10. E2EE polls (`/poll "question" "a" "b"`)
- [ ] Votes ride as signed mesh messages, tally kept client-side with epoch-style convergence
- [ ] Rendered as a single live-updating message row with bar meters (zero server trust)
- [ ] Tests: tally convergence, duplicate-vote handling, row rendering

## 11. Shared room scratchpad (CRDT notepad in the drawer)
- [ ] `#notes` drawer next to files/queue: collaborative text over the mesh (LWW or simple CRDT)
- [ ] Persisted per-room in Redis with TTL (E2EE, ephemeral)
- [ ] Public-room notes vs private/DM notes accessible differently (scope-aware access)
- [ ] Tests: convergence, TTL expiry, scope separation

## 12. Screen share as "just another camera" (OBS-style)
- [ ] Screen source (ffmpeg x11grab/gdigrab) as alternate mediaManager input behind `/share`
- [ ] Reuse announce → publish → watch, generation counters, stale-loop kills, late-joiner admission
- [ ] OBS-style: source selection (screen/window), renders in existing Live Cameras strip
- [ ] Tests: source switch, stop-withdraw, late-joiner admission to screen stream

## 13. Reactions + threaded replies
- [ ] Small protocol additions (react / reply-to message kinds)
- [ ] Rendered inline on existing rows (no new chrome)
- [ ] Tests: reaction aggregation, reply threading, row rendering
