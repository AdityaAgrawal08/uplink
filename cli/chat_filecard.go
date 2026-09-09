package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// ---- file attachment card ----------------------------------------------------
//
// Renders uploaded files as styled attachment cards instead of plain text
// messages. The card shows a file-type icon, filename, extension, size,
// and a download indicator, similar to modern chat apps.

var (
	// Card container style: subtle rounded border with slight padding.
	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	// Card header style: bold filename with accent color.
	cardHeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")) // bright white

	// Card metadata style: faint secondary info (extension, size).
	cardMetaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("247")) // light gray

	// Card icon style: colored file-type icon.
	cardIconStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39")) // blue

	// Card action style: download indicator.
	cardActionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("247")) // light gray

	// Card timestamp style.
	cardTimeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("247")) // light gray
)

// fileIcon returns a Unicode icon appropriate for the given file extension.
func fileIcon(ext string) string {
	switch strings.ToLower(ext) {
	// Documents
	case ".pdf":
		return "📄"
	case ".doc", ".docx":
		return "📝"
	case ".xls", ".xlsx", ".csv":
		return "📊"
	case ".ppt", ".pptx":
		return "📑"
	case ".txt", ".md", ".rst":
		return "📃"
	// Images
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".svg", ".webp", ".ico":
		return "🖼️"
	// Video
	case ".mp4", ".avi", ".mkv", ".mov", ".wmv", ".flv", ".webm":
		return "🎬"
	// Audio
	case ".mp3", ".wav", ".ogg", ".flac", ".aac", ".wma":
		return "🎵"
	// Archives
	case ".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz":
		return "📦"
	// Code
	case ".go", ".js", ".ts", ".py", ".rs", ".java", ".c", ".cpp", ".h",
		".html", ".css", ".json", ".yaml", ".yml", ".toml", ".xml":
		return "💻"
	// Executables
	case ".exe", ".msi", ".dmg", ".app", ".deb", ".rpm":
		return "⚙️"
	// Default
	default:
		return "📎"
	}
}

// fileExtLabel returns the uppercase extension without the dot (e.g. "PDF").
func fileExtLabel(ext string) string {
	if ext == "" {
		return "FILE"
	}
	return strings.ToUpper(strings.TrimPrefix(ext, "."))
}

// truncateFilename shortens a filename to maxLen, preserving the extension.
func truncateFilename(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	ext := filepath.Ext(name)
	stem := name[:len(name)-len(ext)]
	// Reserve room for "…" + extension
	avail := maxLen - len(ext) - 1
	if avail < 4 {
		avail = 4
	}
	return stem[:avail] + "…" + ext
}

// fileAttachmentCard renders a multi-line styled card for a file upload.
// The card layout:
//
//	┌─ 📄 filename.docx ─────────────────────┐
//	│  DOCX · 48 KB                    ↓ save │
//	│                         10:30 PM         │
//	└─────────────────────────────────────────┘
func fileAttachmentCard(filename, username, sizeStr, timestamp string, width int) string {
	ext := filepath.Ext(filename)
	icon := fileIcon(ext)
	label := fileExtLabel(ext)
	dispName := truncateFilename(filename, width-6)

	// Header line: icon + filename
	header := cardIconStyle.Render(icon+" ") + cardHeaderStyle.Render(dispName)

	// Meta line: EXT · SIZE
	meta := cardMetaStyle.Render(fmt.Sprintf("%s · %s", label, sizeStr))

	// Action: download indicator (right-aligned concept, placed on meta line)
	action := cardActionStyle.Render("↓ save")

	// Build the inner content.
	inner := fmt.Sprintf("%s\n%s          %s", header, meta, action)

	return cardStyle.Render(inner)
}
