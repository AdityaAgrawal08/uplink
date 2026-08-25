# Architectural Changes Plan

## Overview
Three enhancements to the uplink-delta chat system:
1. Hide own Entry/Exit system messages from the viewing user
2. Right-side user roster column showing all active participants
3. Click-to-chat: users can click each other for private direct messages

---

## Change 1: Hide Own Entry/Exit Comments

### Problem
When a user joins a session, a system message `"username joined"` appears. When they leave, `"username left"` appears. Currently, a user CAN see their own entry/exit messages, which is confusing and redundant.

### Solution
Filter out system messages where the username mentioned in the text matches the current viewing user.

### Files to Modify

#### Server-side (`src/app/api/v1/session/[sessionId]/join/route.ts`)
- **Line 102**: Change system message to include a `targetUser` field or modify format so client can identify whose entry it is.
- **Current**: `await appendMessage(db, sessionId, "system", "system", `${username} joined`);`
- **Option A**: Store username in message text but client filters it
- **Option B**: Only broadcast join/leave to OTHER participants (not the joining/leaving user)

#### Server-side (`src/app/api/v1/session/[sessionId]/leave/route.ts`)
- **Line 35**: Same as above for leave messages.
- **Current**: `await appendMessage(db, sessionId, "system", "system", `${username} left`);`

#### Client-side TUI (`cli/chat_tui.go`)
- **`renderLine` function (lines 95-108)**: Add filtering logic to skip rendering system messages where the username in the text matches `c.me`.
- **Key insight**: System messages have `Username: "system"` but `text: "alice joined"`. We need to check if `text` contains `c.me` and skip rendering.

#### Client-side Plain mode (`cli/chat.go`)
- **`runChatPlain` function (lines 178-283)**: Add similar filtering in `printMsg` callback.
- **Lines 181-188**: Check if message is a system message mentioning the current user and skip.

### Implementation Detail
System message format: `{username} joined` / `{username} left`
Client check: if `m.Kind == "system" && strings.Contains(m.Text, c.me) && m.Text != ""` → skip rendering

This is preferred over server-side changes because:
- No API contract changes needed
- Works for both TUI and plain modes
- Minimal risk of breaking other functionality

---

## Change 2: Right-Side User Roster Column

### Problem
No visibility into who else is in the session. Users have no way to know who else is present.

### Solution
Add a dedicated right-side column in the TUI showing active participants, similar to OpenCode's sidebar showing session name, context, TODO.

### Files to Modify

#### TUI Model (`cli/chat_tui.go`)
- **`chatScreen` struct** (lines 60-75): Add `showRoster bool` toggle and `rosterWidth int` configuration.
- **New field**: `activeUsers []string` - already tracked via heartbeat, but ensure it's updated.

#### TUI View/Update (`cli/chat_tui.go`)
- **`Init()`** (lines 196-198): No changes needed - heartbeat already fetches active users.
- **`Update()`** (lines 204-348): Add routing for roster display.
- **`View()`** (lines 350-359): restructure layout to have three panes:
  - Header (top)
  - Left: Messages viewport
  - Right: User roster
  - Bottom: Input

- **Layout calculation**: 
  - Total width = c.width
  - Header takes full width but we'll adjust
  - Messages viewport: width = c.width - 2 - rosterWidth
  - Roster column: width = rosterWidth (e.g., 20 chars)
  - Input: width = c.width - 2

#### Rendering the Roster (`cli/chat_tui.go`)
- **New method `rosterView()`**: Display list of active users
  ```
  ╭────────────── Users ──────────────╮
  │ alice                              │
  │ bob                                │
  │                                    │
  └────────────────────────────────────┘
  ```
- Highlight the current user's name differently

- **`appendLine` adjustments**: When adding a line, check if we need to also update the roster view (or keep roster updated via heartbeat refresh).

#### Heartbeat Integration
- The `beatOnce` → `beatDone` already updates `c.users` (line 265-267 in chat_tui.go)
- No changes needed to protocol - just ensure the roster view refreshes on each heartbeat tick

### Design Specifications

**Roster Width**: 20 characters (configurable via `tuiRosterWidth` env var or constant)

**Roster Content**:
- Header: "Users" 
- One line per active user
- Current user marked with "↳" prefix or different color
- Empty state: "No other users"

**Styling**:
- Use existing `tuiNameStyle` or create `tuiRosterStyle`
- Current user: `tuiMeStyle` prefix
- Others: default foreground

**Layout** (ASCII art representation):
```
╭─────────────────────────────────────────────╮
│ uplink chat · key abc123 · you are alice · 3 online │
├─────────────────────────────────────────────┤
│ ✦ alice  (you)                           ✦ │
│ ✦ bob                                    │ │ Messages...
│ ✦ charlie                                │ │         │
│                                         │ │         │
│ ───────────────────────────────────────────── │ │         │
│                                             │ │         │
│ [message1]                                │ │         │
│ [message2]                                │ │         │
│                                             │ ╞══════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════════
│ > Type a message…                          │
╰─────────────────────────────────────────────╯
```

**Alternative Simpler Layout** (if space is tight):
- Collapsible roster (toggle with `/roster` command)
- Fixed width of 15 chars
- Always visible

---

## Change 3: Click-to-Chat (Private Direct Messages)

### Problem
No way for users to have private conversations within a session. All messages are broadcast to everyone.

### Solution
Implement a "select user → private mode" where messages are sent directly to one other user rather than broadcast to all.

### Files to Modify

#### Message Structure (`cli/chat.go` and `cli/chat_tui.go`)
- Extend `chatMessage` to include optional `targetUsername` field
- Or create a separate `privateMessage` type

#### TUI Send Logic (`cli/chat_tui.go`)
- **`doSend`** (lines 166-172): When a target user is selected, prefix message or send via different API endpoint
- **Default**: broadcast to all (current behavior)
- **Private mode**: send to specific user only

#### User Selection UI (`cli/chat_tui.go`)
- **Click handler**: When user clicks on another user's name in the roster, set `c.selectedTarget = username`
- **Visual indicator**: Show which user is selected for private chat
- **Toggle**: Click again to deselect (return to broadcast mode)

#### Message Routing
Two approaches:

**Approach A: Server-supported private messages**
- Modify `POST /api/v1/session/[sessionId]/messages` to accept optional `targetUsername`
- Server forwards message only to that user
- Client displays with special "→ bob" indicator

**Approach B: Client-side filtering (simpler, no server changes)**
- Send all messages to server as broadcast
- Client-side: when `targetUsername` is set, only display messages where `Username == targetUsername || Username == me`
- Other users' messages to the target are hidden from view
- Simpler but doesn't actually restrict server-side broadcasting

**Recommendation**: Start with **Approach B** for MVP - no server changes needed, can be enhanced later.

#### Plain Mode (`cli/chat.go`)
- Similar targeting logic in `printMsg`
- `/p` command to toggle private mode to specific user
- Messages from/private to selected user shown differently

#### Commands
- `/users` - show online users (already exists)
- `/roster` - toggle roster visibility
- `/pm <username>` - start private chat with user
- Click roster user = same as `/pm <username>`

### Implementation Priority
1. Basic click-to-select user (UI only, no private routing yet)
2. Private message send logic (Approach B - client-side filtering)
3. Server enhancement (Approach A) for future

---

## Summary of File Changes

| File | Change Type |
|------|-------------|
| `cli/chat_tui.go` | Major: renderLine filtering, roster column, click handling, send logic |
| `cli/chat.go` | Plain mode: filtering, command updates |
| `src/app/api/v1/session/[sessionId]/join/route.ts` | Optional: message format if needed |
| `src/app/api/v1/session/[sessionId]/leave/route.ts` | Optional: message format if needed |

## Non-Goals (Out of Scope)
- End-to-end encryption for private messages
- Persistent private chat history
- Notification sounds for private messages
- Voice/video calling

## Testing Considerations
- Verify own entry/exit messages disappear for the joining/leaving user
- Verify roster displays correct count and usernames
- Verify clicking a user selects them for private mode
- Verify `/exit` still works in all modes
- Verify `/users` command still works
- Test with 3+ users to ensure message filtering is correct

## Rollback Plan
If any change causes issues:
- Toggle roster with `/roster` command
- Private mode can be disabled by clicking current user or typing `/pm` with no args
- Message filtering can be disabled by commenting out the `strings.Contains` check
- All changes are additive - no existing functionality removed