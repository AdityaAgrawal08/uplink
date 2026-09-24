# UPLINK-Delta — File Map (exhaustive)

> Every file and directory in the repo, what it does, and where to change
> what. Line counts and symbol lists describe the **`UI` branch tip**
> (`3765ba5`). Files untouched by the UI branch match `main`.
> Convention per entry: `PATH | LINES | PURPOSE | KEY SYMBOLS`.
> How to use this file: find the area below, read the listed files, then
> implement — it replaces grep-first navigation.

## Directory overview

```text
cli/                 Go CLI — chat TUI, voice calls, P2P engine, transfers
cli/lan/             mDNS discovery + ephemeral-TLS file server/client
cli/wan/             libp2p DHT wide-area transfer
cli/pkg/crc64        NVMe CRC64 helper
cli/pkg/tarball      Safe directory pack/unpack
src/app/api/v1/      19 Next.js API routes (admin/cleanup/mock-r2/session/share/speedtest)
src/app/share/       Share landing + preview pages
src/components/      FilePreview, SyntaxHighlighter (React)
src/lib/             Backend libraries (auth/rooms/redis/r2/crypto/quota/mongo/env/utils/crc64)
server/              SHELVED Go WebSocket relay (own module) — not deployed
scratch/             Dev e2e shell scripts + debug probes (not shipped)
packaging/           Arch Linux PKGBUILD
.github/workflows/  ci.yml, release.yml, npm.yml
docs/                Documentatio.md (this project manual) + FILEMAP.md (this file)
```

## 1. CLI root — chat TUI (`cli/chat_*.go`)

- `cli/chat.go` | 139 | Routes chat between fullscreen TUI and plain mode. | `chatMessage, conversationKey, generalConv, runChat, runChatPlain`
- `cli/chat_commands.go` | 678 | Slash-command registry (7 commands), palette, member picker, settled layout. | `slashCommands, rankSlashCommands, paletteState, paletteView, handlePaletteKeys, runCommand, modTarget, layoutFor, drawerView`
- `cli/chat_conv_test.go` | 211 | DM isolation from general view and navigation. | `TestDMNeverPaintsInGeneralView, TestThreadViewIsolatedFromRoom, TestMultipleIndependentThreads, TestPendingEchoLivesInItsConversation, TestEnterPrivateSwitchesInstantly`
- `cli/chat_download.go` | 43 | Downloads dir resolution + unique file paths. | `downloadsDir, uniquePath`
- `cli/chat_filecard.go` | 247 | File attachment cards, icons, rune-safe truncation. | `fileIcon, fileExtLabel, truncateFilename, fileAttachmentCard, fileKindLabel`
- `cli/chat_filecard_test.go` | 28 | Rune-safe truncation (emoji/CJK/ASCII). | `TestTruncateFilenameRuneSafe`
- `cli/chat_hover_test.go` | 116 | Hover highlights exactly one roster row. | `TestHoverPaintsExactlyOneRow, TestMotionUpdatesAndClearsHover`
- `cli/chat_markdown.go` | 100 | Terminal-escape sanitizer + lightweight markdown. | `sanitizeDisplay, renderMarkdown`
- `cli/chat_picker.go` | 1031 | File-browser drawer for uploads/downloads/buffers. | `pickerEntry, pickerState, openPicker, loadPickerDir, humanSize, pickerView`
- `cli/chat_picker_test.go` | 564 | Picker browsing, buffering, upload pipeline, drawer. | `TestListDirDirsFirstWithDotfiles, TestParentDirAndBreadcrumb, TestPickerRangeMath, TestPickerBrowseBufferAndQuickUpload`
- `cli/chat_privacy_test.go` | 186 | Private-view filtering, echoes, targeting. | `TestPrivateViewRetroFiltersHistory, TestPendingEchoSurvivesModeSwitch, TestSendTargetsCurrentPeer, TestCommonRoomSendsBroadcast`
- `cli/chat_repro_test.go` | 77 | DM routing over the wire through real Update pipeline. | `TestReproDMRoutingOverWire`
- `cli/chat_sanitize_test.go` | 44 | Escape stripping, markdown attack neutralization. | `TestSanitizeDisplayStripsEscapes, TestRenderMarkdownNeutralizesTerminalAttacks`
- `cli/chat_theme.go` | 583 | Pure presentation theme: sidebar, header, bubbles, composer, system card. No video/tabs/panel/Send/call-card code remains. | `bubbleRatioFor, avatarColorFor, avatarCell, chatItem, chatItems, topBarView, roomHeaderView, roomHeaderCompact, keyHintsView, renderSystemCard, convPreview`
- `cli/chat_theme_test.go` | 453 | Density, headers, clean-UI and no-surface contracts. | `TestAudioStateSurvivesLayout, TestNoVideoSurfaces, TestNoCallCard, TestSidebarPreviewSanitized, TestTopBarCollapsesByWidth, TestComposerFullWidthNoSend, TestRoomTabsRemoved, TestSystemCardGreenBar, TestResizeSweepExactFrame`
- `cli/chat_tui.go` | 2907 | Full-screen chat model: Update/View, mouse, viewports, outbox/settle, audio toggles, leave. | `chatScreen, layout, computeLayout, layoutFor, newChatScreen, orderedUsers, Update, View, runChatTUI, currentScope, toggleAudio, callParties, settleSend, tryEnqueue, maxOutbox`
- `cli/chat_tui_test.go` | 1052 | Layout, viewport, sending, outbox, alerts, wire integration. | `newFilterScreen, wireTestEngine, TestComputeLayout, TestFrameNeverExceedsTerminal, TestTryEnqueue, TestSettle410DrainsOutbox, TestOutboxCap, TestLeaveGuardResets, TestRosterTickBareScreen, TestSearchHeightAgreement, TestNotesParkOnStatusLine, TestServerDownAlertAcrossActions, TestMediaInfoStaysOutOfTranscript`
- `cli/chat_ui_test.go` | 146 | Sidebar density, composer sizing, fullscreen frame. | `TestSidebarWidthDensity, TestComposerRowsDensity, TestFullScreenFrame, TestSidebarSectionsAndNavigation, TestComposerGrowsAndShrinks`
- `cli/chat_unread_test.go` | 221 | Recency ordering, unread badges, roster pruning. | `TestOrderedUsersRecency, TestUnreadLifecycle, TestSidebarRendersAndClearsBadge, TestBeatPrunesDepartedPeers, TestMouseFollowsRecencyOrder`
- `cli/chat_upload.go` | 314 | Sequential P2P uploads with tarballing and progress. | `uploadMaxBytes, uploadJob, uploadState, uploadProgressMsg, startUploads, runSessionUpload, tarballDir`

## 2. CLI root — voice calls / media (`cli/media_*.go`)

Video calling is fully removed on this branch. Audio-only.

- `cli/media_call.go` | 1517 | Audio publish manager: mic toggle, scopes, announces, healing, stops. | `mediaManager, newMediaManager, ToggleAudio, PublishTo, publishAnnounce, sendAnnounce, sendStop, stopAudioPublish, stopAll, MediaActive, AudioScope, healthCheck, currentSendScope`
- `cli/media_call_test.go` | 412 | Audio loopback, solo preview, top-up, mapping, keepalive, late-join hear. | `testSpeaker, publishPair, newPublishPair, TestAudioPublishLoopback, TestToggleAudioSoloPreview, TestPublishTopUpDedupes, TestHiddenVideoAndChatMapping, TestKeepaliveOnStalledMic, TestLateJoinerSeesRunningPublisher, TestSlashRegistryExact`
- `cli/media_call_signal_test.go` | 261 | Media publish over the real signal path end-to-end. | `waitMediaPeer, TestPublishOverSignalPath, TestAudioEndToEndWithTestMic`
- `cli/media_audit_test.go` | 388 | Prune grace, mic-fail withdraw, speaker death. | `TestPruneGraceKeepsFlappingPeer, TestFailedMicStartWithdrawsWatchers, TestSpeakerDeathEmits`
- `cli/media_audio.go` | 635 | Opus voice, mic capture backends, speaker playout. | `voiceRate, opusVoice, newOpusVoice, frameChunker, micCapture, micCandidates, micSource, openMicResilient, requireFFmpeg, speaker`
- `cli/media_audio_test.go` | 216 | Opus roundtrip, chunking, resampling, jitter, mic-fail nil-safety. | `TestOpusVoiceRoundtrip, TestFrameChunkerExact, TestDownmixResample, TestJitterInOrderWithLoss, TestMicSourceFailureNilSafe`
- `cli/media_audio_alsa.go` | 91 | Linux microphone capture via pure-Go ALSA. | `openAlsaMic`
- `cli/media_audio_noalsa.go` | 11 | Non-Linux ALSA stub (returns Linux-only error). | `openAlsaMic`
- `cli/media_codec.go` | 61 | Audio packet codec + transport dispatch (audio/keyreq/ping/pong). | `audioPacket, encodeAudioPacket, decodeAudioPacket, dispatchMedia`
- `cli/media_jitter.go` | 126 | Voice jitter buffer (order across loss/reorder). | `jitterBuffer, newJitterBuffer, push, pop, resyncLocked`
- `cli/media_latejoin_test.go` | 159 | Audio late-join over the real signal path. | `TestLateJoinerThreePartySignal`
- `cli/media_transport.go` | 739 | Encrypted media datagrams over direct UDP (Noise sessions, replay cache). | `dgramCipher, mediaPeer, mediaTransport, mediaCallbacks, newMediaTransport, sendMedia, onDatagram, peerReady`
- `cli/media_transport_test.go` | 481 | UDP handshake, key-swap rejection, cipher replay, codec, nominate. | `loopbackPair, waitMediaReady, TestMediaHandshakeLoopback, TestMediaKeySwapRejected, TestDgramCipherReplay, TestNominateSkipsUnroutable`

## 3. CLI root — P2P engine and protocol (`cli/p2p_*.go`)

- `cli/p2p_box.go` | 99 | Async E2E pairwise boxes (static ECDH → AES-GCM) for inbox fallback. | `deriveBoxKey, sealBox, openBox`
- `cli/p2p_box_test.go` | 87 | Box round-trip, tamper, wrong-peer, nonce, inputs. | `TestBoxRoundTrip, TestBoxTamperRejected, TestBoxWrongPeerFails, TestBoxNonceRandomness`
- `cli/p2p_engine.go` | 1560 | Mesh+Noise orchestration, heartbeats, inbox, files, roster, beat-error record. | `engineChat, engineFile, engineCallbacks, engine, newEngine, sendChat, sendFile, beatErr, saveVerifiedFile, safeDestName, maxFileAssemblies, fileAssembly, PublishTo`
- `cli/p2p_engine_test.go` | 460 | Engine e2e, assembly bounds, size checks, beat failures. | `TestEngineEndToEnd, TestAssemblyBounds, TestSaveVerifiedSizeMismatch, TestSafeDestName, TestBeatRecordsAndClearsFailures`
- `cli/p2p_mesh.go` | 552 | WebRTC mesh transport, deterministic offer roles, STUN. | `meshLabel, resolveStunURLs, meshCallbacks, meshPeer, mesh, newMesh, ensurePeer`
- `cli/p2p_mesh_test.go` | 186 | Mesh loopback messaging and tie-break roles. | `TestMeshLoopback, TestMeshTieBreak`
- `cli/p2p_noise.go` | 211 | Noise_XX E2E sessions per peer pair. | `identityKey, generateIdentity, peerSession, beginNoise, stepNoise`
- `cli/p2p_noise_test.go` | 150 | Handshake round-trip, tamper, guards, safety codes. | `TestNoiseHandshakeRoundTrip, TestNoiseTamperRejected, TestSafetyCodeAgainstHandshake`
- `cli/p2p_proto.go` | 160 | JSON frame protocol, chunking, safety codes, seen-sets. | `frameChat, frameAck, frameChunkSize, frame, newFrame, encodeFrame, decodeFrame, newMsgId`
- `cli/p2p_proto_test.go` | 141 | Frame encoding, IDs, dedup, chunking, codes. | `TestFrameRoundTrip, TestDecodeFrameRejects, TestNewMsgIdUnique, TestSeenSetDedupAndEvict, TestSplitChunks`
- `cli/p2p_signal.go` | 356 | Thin HTTP signaling client + server-down classifier. | `rosterMember, signalNote, inboxBox, signalClient, createRoom, joinRoom, signalSend, isServerDown, serverDownMsg, apiStatusCode`
- `cli/p2p_signal_test.go` | 453 | Fake signaling server + full client flow + classifier. | `fakeSignalServer, TestSignalFullFlow, TestIsServerDown, TestIsServerDownExcludesMesh`
- `cli/p2p_stability_test.go` | 1519 | Handshake robustness, retries, backpressure, receipts, header contract. | `TestDuplicateNoise1IgnoredAfterCompletion, TestFlapCounting, TestDepartedUserCleanup, TestHeaderStaysClean`

## 4. CLI root — transfers, entrypoint, support

- `cli/main.go` | 1595 | Entrypoint: `send`/`receive`/`create session`/`join`/`config`/`version`/`update`, E2EE cloud flow, LAN/WAN dispatch. | `ShareMeta, main, printUsage, normalizeFlagOrder, generateShareCode, handleSend, handleReceive, cmdCreateSession`
- `cli/main_test.go` | 292 | Share codes, filename sanitizing, chunking, flag order. | `TestGenerateShareCode, TestSanitizeFilename, TestChunkSizeFloor, TestNormalizeFlagOrder, TestAdaptiveChunkerClamping`
- `cli/download.go` | 186 | Resumable downloads with hash verification + checkpoints. | `DownloadResumable`
- `cli/resume.go` | 112 | Multipart upload resume state + cleanup. | `ResumeState, Save, LoadResumeState, DeleteResumeState, CleanOldResumeStates`
- `cli/speed.go` | 72 | Bandwidth measure + adaptive multipart chunk sizing. | `AdaptiveChunker, Measure, RecordSpeed, ChunkSize`
- `cli/session.go` | 219 | Create/join prompts, shared HTTP client, JSON helpers. | `sharedHTTPClient, postJSON, getJSON, cmdCreateSession, cmdJoinChat`
- `cli/landing.go` | 816 | CREATE/JOIN landing TUI form + validation + API calls. | `landingModel, newLandingModel, Init, Update`
- `cli/landing_test.go` | 66 | Landing code/username validation. | `TestLandingValidateCodeStrict, TestLandingValidateUsername`
- `cli/config.go` | 97 | Layered config: defaults < file < env. | `Config, defaultConfig, LoadConfig`
- `cli/config_cmd.go` | 175 | `config get/set/ls` subcommands. | `handleConfig, handleConfigGet, handleConfigSet, handleConfigLs`
- `cli/encrypt.go` | 160 | Streaming AES-GCM file encrypt/decrypt in chunks. | `EncryptFileStream, DecryptFileStream`
- `cli/qr.go` | 55 | Terminal QR rendering (capability-gated). | `ShouldShowQR, PrintQRCode`
- `cli/clipboard.go` | 11 | Silent system-clipboard copy. | `copyToClipboard`
- `cli/notify.go` | 15 | Desktop notifications for transfer completion/failure. | `notifyTransferComplete, notifyTransferFailed`
- `cli/version.go` | 462 | Version display, update check, checksum-verified install. | `handleVersion, normVersion, handleUpdate, installBinary, verifyReleaseChecksum`
- `cli/version_update_test.go` | 198 | Release extraction, versions, auth headers, zip-slip. | `TestExtractReleaseAssetTar, TestExtractReleaseAssetZipSlip, TestCmpVersions`
- `cli/scrollbars_test.go` | 149 | Scrollbar geometry + independent pane scrolling. | `geomScreen, TestScrollbarDragGeometry, TestIndependentScrollPanes, TestRosterScrollKeepsSelection`
- `cli/go.mod` | — | Module `github.com/AdityaAgrawal08/uplink-delta/cli`, go 1.27. Direct deps: charmbracelet (bubbles/bubbletea/lipgloss/termenv), flynn/noise, gen2brain/beeep, hashicorp/mdns, ipfs/go-cid, libp2p + kad-dht, multiformats/go-multihash, pion/rtp + webrtc/v4, skip2/go-qrcode, tphakala/go-audio-capture + go-opus, atotto/clipboard, x/crypto, x/term. All imported (tidy is a no-op). |

## 5. CLI subpackages

- `cli/lan/discovery.go` | 108 | mDNS advertise/discover of LAN shares. | `ServiceInfo, RegisterService, DiscoverService`
- `cli/lan/server.go` | 156 | Ephemeral-TLS HTTPS file server with limits. | `ActiveConnections, ServeFileLAN, ServeFileLANWithProgress`
- `cli/lan/tls.go` | 60 | Ephemeral certificate generation + fingerprints. | `GenerateEphemeralCert`
- `cli/lan/transfer.go` | 106 | LAN download with fingerprint + hash verification. | `DownloadFileLAN`
- `cli/wan/wan.go` | 190 | libp2p DHT WAN transfer (rendezvous by CID). | `deriveCID, StartWANPeer, ServeFileWAN, DownloadFileWAN`
- `cli/pkg/crc64/crc64.go` | 69 | NVMe CRC64 with table generation (+ `crc64_test.go`, 29 lines, `TestCRC64NVMe`). | `MakeTable, TableNVME, New, Sum64`
- `cli/pkg/tarball/tarball.go` | 216 | Directory pack/unpack with exclusions + slip/bomb limits (+ `tarball_test.go`, 137 lines). | `Pack, Unpack, UnpackLimit`

## 6. Web API routes (`src/app/api/v1/`)

- `admin/quota/route.ts` | 55 | `GET` R2 quota usage + upload status (admin bearer). |
- `cleanup/route.ts` | 187 | `POST`(+`GET`) expired-share sweep with atomic locking. | `performCleanup`
- `mock-r2-download/route.ts` | 72 | `GET` local-file dev downloads. | dev only |
- `mock-r2-upload/route.ts` | 80 | `PUT` local-buffer dev uploads. | dev only |
- `session/[sessionId]/admin/route.ts` | 43 | `POST` grant/revoke admin (creator only). |
- `session/[sessionId]/heartbeat/route.ts` | 37 | `POST` presence + live roster with epoch. |
- `session/[sessionId]/inbox/route.ts` | 70 | `POST` deposit offline boxes, `GET` fetch without deletion. |
- `session/[sessionId]/inbox/ack/route.ts` | 30 | `POST` delete acknowledged boxes + counts. |
- `session/[sessionId]/join/route.ts` | 52 | `POST` join after code + password validation. |
- `session/[sessionId]/kick/route.ts` | 43 | `POST` remove + ban (role-checked). |
- `session/[sessionId]/leave/route.ts` | 26 | `POST` remove member, destroy emptied rooms. |
- `session/[sessionId]/signal/route.ts` | 63 | `POST`+`GET` WebRTC note queues per user. |
- `session/cleanup/route.ts` | 33 | `GET`+`POST` stale-room sweep (cron). | `performSessionCleanup`
- `session/create/route.ts` | 47 | `POST` create room, optional password. |
- `share/[id]/route.ts` | 71 | `GET` public share metadata + expiry. |
- `share/[id]/authorize-download/route.ts` | 207 | `POST` password verify + presigned URL. |
- `share/[id]/confirm/route.ts` | 324 | `POST` verify completion + activate share. |
- `share/[id]/parts/route.ts` | 100 | `GET` multipart parts list (resume). |
- `share/[id]/preview-text/route.ts` | 91 | `POST` truncated text preview (authed). |
- `share/init/route.ts` | 334 | `POST` init share, reserve quota, issue upload URLs. |
- `speedtest/route.ts` | 36 | `GET` random bytes, rate-limited. |

## 7. Web app + libraries (`src/`)

- `app/layout.tsx` | 33 | Root HTML layout, fonts, metadata. | `RootLayout`
- `app/page.tsx` | 17 | CLI-only landing + install instructions. | `Home`
- `app/share/[id]/page.tsx` | 60 | Share metadata loader + preview renderer. | `SharePage`
- `components/FilePreview.tsx` | 391 | Preview, password gate, QR code, download flow. | `FilePreview`
- `components/SyntaxHighlighter.tsx` | 35 | Safe highlight.js code rendering. | `SyntaxHighlighter`
- `lib/api-utils.ts` | 27 | JSON error + safe body-parse helpers. | `apiError, parseJsonBody`
- `lib/auth.ts` | 25 | Admin bearer validation (constant-time). | `validateAdminAuth`
- `lib/crc64.ts` | 31 | CRC64-NVME checksum utilities. | `crc64nvme, crc64nvmeBase64`
- `lib/crypto.ts` | 72 | Password hashing, IP anonymization, IDs, filenames. | `hashPassword, verifyPassword, anonymizeIp, generateShareId, sanitizeFilename`
- `lib/env.ts` | 59 | Required/signaling env validation. | `validateEnv, validateSignalingEnv`
- `lib/mongodb.ts` | 140 | Mongo connection, indexes, quota init. | `getDb, initIndexes`
- `lib/quota.ts` | 394 | R2 storage + ops quota ledger in Mongo. | `getQuotaState, reserveUploadQuota, consumeClassBQuota`
- `lib/r2.ts` | 368 | S3/R2 client, presigned multipart ops (+mock). | `getPresignedUploadUrl, getPresignedDownloadUrl, checkObjectExists, completeMultipartUpload`
- `lib/redis.ts` | 501 | Upstash client + in-memory mock + lazy wrapper. | `MockRedis, LazyRedisClient, redis`
- `lib/rooms.ts` | 839 | Redis signaling rooms: presence, messaging, roles. | `createRoom, joinRoom, leaveRoom, heartbeat, depositSignal, drainSignals, depositBox, sweepRooms`
- `lib/__tests__/crypto.test.ts` | 32 | Filename-sanitization parity with Go. |
- `lib/__tests__/redis.test.ts` | 167 | MockRedis semantics + prod gating. |
- `lib/__tests__/rooms.test.ts` | 321 | Rooms plane integration (CRUD, heartbeat, signals). |
- Root web config: `package.json` (Next.js app; deps: aws-sdk×2, upstash/redis, hash-wasm, highlight.js, mongodb, next, qrcode, react×2; dev: tailwind, vitest, types, eslint), `tsconfig.json`, `next.config.ts` (security headers/CSP), `postcss.config.mjs`, `vercel.json` (region/timeouts/cleanup cron), `eslint.config.mjs`.

## 8. Shelved server + infra + tooling

- `server/main.go` | 46 | HTTP server start, routes, cleaner, shutdown. | `main`
- `server/server.go` | 696 | Session handlers, WebSocket pumps, chat/file relay. | `Server, NewServer, handleCreateSession, handleWSUpgrade, readPump, writePump`
- `server/types.go` | 251 | Wire structs, session state, ring buffer. | `Inbound, Outbound, ChatMessage, RingBuffer, Connection, Session`
- `server/crypto.go` | 66 | X25519 keygen, shared secret, HKDF. | `GenerateKeyPair, DeriveSharedSecret, DeriveEncryptionKey`
- `server/util.go` | 137 | ID/JSON helpers, env config, shutdown. | `genId, Config, DefaultConfig, loadConfig, isValidUsername`
- `server/ratelimit.go` | 63 | Per-key sliding-window limiter + cleanup. | `RateLimiter, Allow`
- `server/{auth,crypto,server}_test.go` + `integration_test.go` | 62/113/212/456 | Password hashing, crypto round-trips, ring buffer, session e2e. | (own `server/go.mod`, go 1.23, gorilla/websocket + x/crypto)
- `Makefile` | build/install/clean + 5-target release tarballs. |
- `.goreleaser.yaml` | Multi-OS builds, archives, checksums for tags `v*`. |
- `.github/workflows/ci.yml` | Push/PR: Go vet + `go test -race` + cross-builds; web lint/typecheck/unit/build; e2e matrix; single `pipeline` gate. No per-run artifact uploads. |
- `.github/workflows/release.yml` | Tag push → GoReleaser publish. |
- `.github/workflows/npm.yml` | Tag push → tag-gated npm publish (`@aditya/uplink` shim). |
- `install.sh` / `install.ps1` | Checksum-verified installers (fail closed, sudo fallback). |
- `packaging/archlinux/PKGBUILD` | Git-snapshot pacman package. |
- `scratch/` | `e2e_phase0.sh`, `session_flow_test.sh`, `chat_two_clients.sh`, `probe_r2.ts`, `test_quota.ts`, `run_tests.ts` — dev e2e/debug scripts (not shipped). |
- `DEPLOY.md`, `.env.example`, `.gitignore` | Deploy guide, env template, ignores (node, `.next`, Go binaries, `.env*`, `uploads_dev`). |
