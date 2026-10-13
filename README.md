# Uplink Delta

Uplink Delta is a private file-sharing and chat tool that runs in your terminal. Share a file or folder and pass along a short code or link, open a room where a group can message, send files, and talk over voice, and keep everything temporary and encrypted between the people involved.

## Features

### File sharing

- Share any file or folder and get a short code and link to pass to the recipient. Folders are packed up automatically.
- Optional end-to-end encryption: the file is encrypted on your device before it is sent, and the key travels only in the link — it never reaches the service.
- Protect a share with a password and an expiry, from minutes to days.
- Downloads are verified for integrity, and large uploads resume if they are interrupted.
- Show a QR code so a phone or another device can pick up the link.
- On the same network, LAN mode sends the file directly between the two devices.

### Chat rooms

- Create a room and share its six-digit code. Anyone with the code can join from anywhere, and a room can be password-protected.
- Rooms are ephemeral: they last while people are in them and end when the last member leaves.
- A full terminal interface with a general room, private one-to-one conversations, unread badges, and desktop notifications.
- Markdown formatting, quote replies, reactions, and @mentions that notify the person mentioned.
- Share files inside a room: browse your computer to upload, or download what others have shared.
- Messages sent to someone who is offline wait for them and arrive once they are back.
- Voice chat in rooms and private conversations (audio only).

### Groups

- Named, invite-based groups inside a room, for conversations that need their own space.
- Admins can rename a group, edit its description, transfer admin rights, and remove members; the creator can delete the group.
- An optional member limit can be set when a group is created.
- Groups you have joined rejoin automatically the next time you open the app.

### Browser links

- A share link opens in any browser with a preview and a download option, so receiving does not require the app.
- Creating shares and using chat rooms happens in the terminal app.

### Install and updates

- One-line installer for Linux and macOS, a PowerShell installer for Windows, with builds for Intel and ARM machines.
- The app can update itself to the latest release, checking what it downloads first.

## How it works

- **No accounts.** On first run the app creates a private identity on your device. There is no sign-up, and nothing is tied to an email address or a password elsewhere.
- **Encrypted between people.** Chat messages, offline messages, and voice are encrypted on one device and can only be read on another. Encrypted file shares work the same way: the key travels in the link, not through the service.
- **The service coordinates, not listens.** It helps devices find each other and holds encrypted messages while someone is offline. It can see room membership, names, presence, and file details such as size and timing, but not message or file contents.
- **Temporary by design.** Rooms close when everyone leaves, and shares stop working when they expire.

## Getting started

Get the latest release from the project's releases page, or run the installer for your platform: the install script on Linux and macOS, the PowerShell script on Windows. Both verify the download before installing.

Then:

- Run `uplink` with no arguments to open the create-or-join screen.
- To share a file, use `uplink send`; it prints a code and a link. The recipient uses `uplink receive` with the code, or opens the link in a browser.
- For a chat room, one person runs `uplink create session` and shares the six-digit code. Everyone else runs `uplink join` with that code.

## Platforms

Linux, macOS, and Windows on Intel and ARM. Voice chat needs a working microphone; everything else is keyboard-only.
