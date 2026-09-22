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
	// Width is set dynamically in fileAttachmentCard so the card hugs content.
	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)

	// Compact card: fixed small padding, no full-width expansion.
	cardStyleCompact = lipgloss.NewStyle().
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
// Rune-based: byte slicing could split a multi-byte rune and emit invalid
// UTF-8 (broken glyphs, width miscalculations, card overflow).
func truncateFilename(name string, maxLen int) string {
	r := []rune(name)
	if len(r) <= maxLen {
		return name
	}
	ext := filepath.Ext(name)
	extR := []rune(ext)
	stemR := r[:len(r)-len(extR)]
	// Reserve room for "…" + extension
	avail := maxLen - len(extR) - 1
	if avail < 4 {
		avail = 4
	}
	if avail > len(stemR) {
		avail = len(stemR)
	}
	return string(stemR[:avail]) + "…" + ext
}

// fileAttachmentCard renders a multi-line styled card for a file upload.
// The card layout (reference: dark tile, file glyph, name + size/mime,
// download affordance on the right):
//
//	┌──────────────────────────────────┐
//	│  🖼  screenshot.png            ⤓  │
//	│      1.4 MB · image/png           │
//	└──────────────────────────────────┘
func fileAttachmentCard(filename, username, sizeStr, timestamp string, width int) string {
	ext := filepath.Ext(filename)
	icon := fileIcon(ext)
	label := fileExtLabel(ext)

	// Compact card: width hugs content, not the viewport.
	// Needed width = icon + filename + padding; cap to keep it small.
	iconW := lipgloss.Width(icon + " ")
	// Start with filename truncated to a compact max (28 chars) rather than viewport width.
	compactMax := 28
	if width > 0 && width < compactMax+10 {
		compactMax = width - 10
		if compactMax < 12 {
			compactMax = 12
		}
	}
	dispName := truncateFilename(filename, compactMax)
	headerPlain := icon + " " + dispName
	headerW := lipgloss.Width(headerPlain)

	metaPlain := fmt.Sprintf("%s · %s", label, sizeStr)
	actionPlain := "↓ save"
	metaW := lipgloss.Width(metaPlain)
	actionW := lipgloss.Width(actionPlain)
	// Card inner content width = max(header, meta+action gap)
	contentW := headerW
	if metaW+4+actionW > contentW {
		contentW = metaW + 4 + actionW
	}
	// Timestamp row width
	tsW := 0
	if timestamp != "" {
		tsW = lipgloss.Width(timestamp)
		if tsW > contentW {
			contentW = tsW
		}
	}
	// Add small padding, cap to keep rectangular small.
	cardInnerW := contentW + 2 // 1 padding each side
	if cardInnerW < 18 {
		cardInnerW = 18
	}
	if cardInnerW > 36 {
		cardInnerW = 36
		// re-truncate filename if needed to fit cap
		if headerW > cardInnerW-2 {
			dispName = truncateFilename(filename, cardInnerW-2-iconW)
			headerPlain = icon + " " + dispName
		}
	}
	// Clamp to viewport if still too wide.
	if width > 0 && cardInnerW+4 > width {
		cardInnerW = width - 4
		if cardInnerW < 18 {
			cardInnerW = 18
		}
	}

	// MIME-flavoured meta: "1.4 MB · image/png".
	mimeMeta := fmt.Sprintf("%s · %s/%s", sizeStr, fileKindLabel(ext), strings.ToLower(label))
	header := thFileDlStyle.Render(icon+" ") + thFileNameStyle.Render(dispName)
	metaLeft := thFileMetaStyle.Render(mimeMeta)
	action := thFileDlStyle.Render("⤓")

	// Name line: icon + name left, download glyph right.
	gapH := cardInnerW - lipgloss.Width(headerPlain) - lipgloss.Width("⤓")
	if gapH < 2 {
		gapH = 2
	}
	nameLine := fmt.Sprintf("%s%s%s", header, strings.Repeat(" ", gapH), action)

	// Meta line: indented under the icon.
	indent := strings.Repeat(" ", lipgloss.Width(icon+" "))
	metaLine := indent + metaLeft

	var inner string
	if timestamp != "" {
		tsRender := cardTimeStyle.Render(timestamp)
		tsPad := cardInnerW - lipgloss.Width(timestamp)
		if tsPad < 0 {
			tsPad = 0
		}
		inner = fmt.Sprintf("%s\n%s\n%s%s", nameLine, metaLine, strings.Repeat(" ", tsPad), tsRender)
	} else {
		inner = fmt.Sprintf("%s\n%s", nameLine, metaLine)
	}
	// Fixed compact width so the card hugs content, never the viewport.
	return thFileCardStyle.Width(cardInnerW + 2).Render(inner)
}

// fileKindLabel maps an extension onto a coarse MIME family for the card's
// meta line (image/png, video/mp4, …).
func fileKindLabel(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".svg", ".webp", ".ico":
		return "image"
	case ".mp4", ".avi", ".mkv", ".mov", ".wmv", ".flv", ".webm":
		return "video"
	case ".mp3", ".wav", ".ogg", ".flac", ".aac", ".wma":
		return "audio"
	case ".pdf":
		return "document"
	case ".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz":
		return "archive"
	default:
		return "file"
	}
}

// alignRight pads s on the left so it appears right-aligned within availWidth.
func alignRight(s string, availWidth int) string {
	w := lipgloss.Width(s)
	if w >= availWidth || availWidth <= 0 {
		return s
	}
	pad := availWidth - w
	return strings.Repeat(" ", pad) + s
}
