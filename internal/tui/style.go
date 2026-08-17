package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/srimajji/dsx/internal/terminal"
)

const (
	maxTUIContentWidth   = 128
	maxSetupContentWidth = 88
	compactTUIWidth      = 32
	compactTUIHeight     = 28
)

type visualTheme struct {
	enabled bool
	accent  lipgloss.Style
	brand   lipgloss.Style
	title   lipgloss.Style
	muted   lipgloss.Style
	section lipgloss.Style
	label   lipgloss.Style
	value   lipgloss.Style
	bullet  lipgloss.Style
	success lipgloss.Style
	warning lipgloss.Style
	danger  lipgloss.Style
	border  lipgloss.Style
	active  lipgloss.Style
	key     lipgloss.Style
}

func newVisualTheme(enabled bool) visualTheme {
	theme := visualTheme{enabled: enabled}
	theme.accent = lipgloss.NewStyle()
	theme.brand = lipgloss.NewStyle().Padding(0, 1)
	theme.title = lipgloss.NewStyle()
	theme.muted = lipgloss.NewStyle()
	theme.section = lipgloss.NewStyle()
	theme.label = lipgloss.NewStyle()
	theme.value = lipgloss.NewStyle()
	theme.bullet = lipgloss.NewStyle()
	theme.success = lipgloss.NewStyle()
	theme.warning = lipgloss.NewStyle()
	theme.danger = lipgloss.NewStyle()
	theme.border = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	theme.active = theme.border.Copy()
	theme.key = lipgloss.NewStyle()
	if !enabled {
		return theme
	}
	accent := lipgloss.Color("#A78BFA")
	cyan := lipgloss.Color("#67E8F9")
	green := lipgloss.Color("#86EFAC")
	yellow := lipgloss.Color("#FDE68A")
	red := lipgloss.Color("#FB7185")
	muted := lipgloss.Color("#858AA0")
	border := lipgloss.Color("#3B4055")
	theme.accent = theme.accent.Bold(true).Foreground(accent)
	theme.brand = theme.brand.Bold(true).Foreground(lipgloss.Color("#11111B")).Background(accent)
	theme.title = theme.title.Bold(true).Foreground(lipgloss.Color("#F4F4F5"))
	theme.section = theme.section.Bold(true).Foreground(cyan)
	theme.label = theme.label.Bold(true).Foreground(accent)
	theme.value = theme.value.Foreground(lipgloss.Color("#E4E4E7"))
	theme.bullet = theme.bullet.Foreground(cyan)
	theme.muted = theme.muted.Foreground(muted)
	theme.success = theme.success.Bold(true).Foreground(green)
	theme.warning = theme.warning.Bold(true).Foreground(yellow)
	theme.danger = theme.danger.Bold(true).Foreground(red)
	theme.border = theme.border.BorderForeground(border)
	theme.active = theme.active.BorderForeground(cyan)
	theme.key = theme.key.Bold(true).Foreground(cyan)
	return theme
}

func tuiContentWidth(width int) int {
	if width <= 0 {
		width = 80
	}
	gutter := 0
	if width >= 32 {
		gutter = 2
	}
	return max(1, min(width-gutter, maxTUIContentWidth))
}

func tuiSetupWidth(width int) int {
	return min(tuiContentWidth(width), maxSetupContentWidth)
}

func compactTUILayout(width, height int) bool {
	return width < compactTUIWidth || height > 0 && height < compactTUIHeight
}

func tuiGap(height int) string {
	if height > 0 && height < compactTUIHeight {
		return "\n"
	}
	return "\n\n"
}

func wrapTUIText(value string, width int) string {
	if width <= 0 || value == "" {
		return ""
	}
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		line = strings.TrimRight(line, " ")
		lines[index] = terminal.Wrap(ansi.Wordwrap(line, width, " "), width)
	}
	return strings.Join(lines, "\n")
}
func (theme visualTheme) layout(content string, terminalWidth int) string {
	return theme.layoutAt(content, terminalWidth, tuiContentWidth(terminalWidth))
}

func (theme visualTheme) layoutAt(content string, terminalWidth, contentWidth int) string {
	leftPadding := max(0, (terminalWidth-contentWidth)/2)
	if leftPadding == 0 {
		return content
	}
	prefix := strings.Repeat(" ", leftPadding)
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if line != "" {
			lines[index] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func (theme visualTheme) center(content string, width int) string {
	content = wrapTUIText(content, width)
	return lipgloss.NewStyle().Width(width).Align(lipgloss.Center).Render(content)
}

func (theme visualTheme) header(section, subtitle string, width int) string {
	brand := theme.brand.Render("DSX")
	section = theme.accent.Render(strings.ToUpper(section))
	left := brand + "  " + section
	if lipgloss.Width(left) > width {
		return brand + "\n" + section
	}
	if subtitle == "" || width < 58 {
		return left
	}
	available := max(1, width-lipgloss.Width(left)-2)
	right := theme.muted.Render(terminal.Truncate(subtitle, available))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Repeat(" ", max(1, width-lipgloss.Width(left)-lipgloss.Width(right))), right)
}

func (theme visualTheme) panelBodyWidth(width, height int) int {
	if compactTUILayout(width, height) {
		return max(1, width)
	}
	return max(1, width-theme.border.GetHorizontalFrameSize())
}

func (theme visualTheme) panel(title, body string, width, height int, active bool) string {
	innerWidth := theme.panelBodyWidth(width, height)
	heading := wrapTUIText(theme.title.Render(title), innerWidth)
	body = wrapTUIText(body, innerWidth)
	if compactTUILayout(width, height) {
		return heading + "\n" + body
	}
	style := theme.border
	if active {
		style = theme.active
	}
	return style.Width(innerWidth).Render(heading + "\n" + body)
}

func (theme visualTheme) stepper(active int, width int) string {
	labels := []string{"Environment", "Review", "Ready"}
	if width < 32 {
		return theme.center(theme.accent.Render(fmt.Sprintf("%d/%d  %s", active+1, len(labels), labels[min(active, len(labels)-1)])), width)
	}
	parts := make([]string, 0, len(labels))
	for index, label := range labels {
		marker := "○"
		style := theme.muted
		if index < active {
			marker = "✓"
			style = theme.success
		} else if index == active {
			marker = "●"
			style = theme.accent
		}
		parts = append(parts, style.Render(fmt.Sprintf("%s %s", marker, label)))
	}
	separator := theme.muted.Render(" ─── ")
	line := strings.Join(parts, separator)
	if lipgloss.Width(line) > width {
		return theme.center(parts[min(active, len(parts)-1)], width)
	}
	return theme.center(line, width)
}

func (theme visualTheme) badge(label, tone string) string {
	style := theme.muted
	switch tone {
	case "success":
		style = theme.success
	case "warning":
		style = theme.warning
	case "danger":
		style = theme.danger
	case "accent":
		style = theme.accent
	}
	return style.Render(label)
}

func (theme visualTheme) help(width int, items ...string) string {
	rendered := make([]string, 0, len(items))
	for _, item := range items {
		key, description, found := strings.Cut(item, " ")
		if !found {
			rendered = append(rendered, theme.key.Render(item))
			continue
		}
		rendered = append(rendered, theme.key.Render(key)+" "+theme.muted.Render(description))
	}
	var output strings.Builder
	lineWidth := 0
	for _, item := range rendered {
		itemWidth := terminal.Width(item)
		if itemWidth > width {
			if lineWidth > 0 {
				output.WriteByte('\n')
				lineWidth = 0
			}
			wrapped := wrapTUIText(item, width)
			output.WriteString(wrapped)
			lines := strings.Split(wrapped, "\n")
			lineWidth = terminal.Width(lines[len(lines)-1])
			continue
		}
		if lineWidth > 0 && lineWidth+2+itemWidth >= width {
			output.WriteByte('\n')
			lineWidth = 0
		}
		if lineWidth > 0 {
			output.WriteString("  ")
			lineWidth += 2
		}
		output.WriteString(item)
		lineWidth += itemWidth
	}
	return output.String()
}

func friendlyProjectName(root string) string {
	name := filepath.Base(root)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "this project"
	}
	return terminal.SanitizeLine(name)
}
