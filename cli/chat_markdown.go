package main

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	mdBoldRe   = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdCodeRe   = regexp.MustCompile("`([^`]+)`")
	mdBlockRe  = regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")
	mdLinkRe   = regexp.MustCompile(`\[(.+?)\]\((.+?)\)`)
)

var (
	mdCodeInlineStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("212")).
				Background(lipgloss.Color("237")).
				Padding(0, 1)
	mdCodeBlockStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252")).
				Background(lipgloss.Color("235")).
				Padding(0, 1).
				Margin(0, 0)
	mdLangStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			Bold(true)
)

// sanitizeDisplay strips terminal escape machinery from untrusted strings
// before they are rendered. lipgloss/bubbletea pass content through, so an
// ESC byte from a peer message or filename would otherwise execute in the
// receiver's terminal (clear screen, title set, hidden text, hyperlink
// injection). Removing the introducer byte is total: without ESC no
// sequence — CSI, OSC, charset, or lone — can function; residue renders as
// harmless visible text. BEL and other C0 controls go too, except \n and
// \t which layout legitimately uses.
func sanitizeDisplay(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x1b || r == 0x07 {
			return -1
		}
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		if r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// renderMarkdown applies lightweight inline markdown to a single line of text.
// Supports: **bold**, `inline code`, [text](url). Code blocks (multi-line)
// are handled at the caller level via renderChatLine.
func renderMarkdown(text string) string {
	// Untrusted input first: peer messages arrive raw over the wire.
	text = sanitizeDisplay(text)
	if strings.HasPrefix(text, "```") {
		// Strip the opening ```lang and closing ```
		lines := strings.SplitN(text, "\n", 2)
		lang := strings.TrimPrefix(lines[0], "```")
		body := text
		if len(lines) > 1 {
			body = strings.TrimSuffix(lines[1], "```")
			body = strings.TrimSuffix(body, "\n")
		}
		rendered := mdCodeBlockStyle.Render(body)
		if lang != "" {
			rendered = mdLangStyle.Render(" "+lang) + "\n" + rendered
		}
		return rendered
	}

	// Inline code: `code`
	text = mdCodeRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[1 : len(m)-1]
		return mdCodeInlineStyle.Render(inner)
	})

	// Bold: **text**
	text = mdBoldRe.ReplaceAllStringFunc(text, func(m string) string {
		inner := m[2 : len(m)-2]
		return lipgloss.NewStyle().Bold(true).Render(inner)
	})

	// Links: [text](url) — render text in accent color
	text = mdLinkRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := mdLinkRe.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render(parts[1]) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Render(" ("+parts[2]+")")
	})

	return text
}
