package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/srimajji/dsx/internal/terminal"
)

const maxTUIContentWidth = 112

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
	theme.border = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
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
	return max(20, min(width-2, maxTUIContentWidth))
}

func (theme visualTheme) layout(content string, width int) string {
	contentWidth := tuiContentWidth(width)
	leftPadding := max(0, (max(width, contentWidth)-contentWidth)/2)
	if leftPadding == 0 {
		return content
	}
	return lipgloss.NewStyle().MarginLeft(leftPadding).Render(content)
}

func (theme visualTheme) center(content string, width int) string {
	contentWidth := tuiContentWidth(width)
	content = terminal.Wrap(content, contentWidth)
	return lipgloss.NewStyle().Width(contentWidth).Align(lipgloss.Center).Render(content)
}

func (theme visualTheme) header(section, subtitle string, width int) string {
	contentWidth := tuiContentWidth(width)
	brand := theme.brand.Render("DSX")
	section = theme.accent.Render(strings.ToUpper(section))
	left := brand + "  " + section
	if subtitle == "" || contentWidth < 58 {
		return left
	}
	available := max(1, contentWidth-lipgloss.Width(left)-2)
	right := theme.muted.Render(terminal.Truncate(subtitle, available))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, strings.Repeat(" ", max(1, contentWidth-lipgloss.Width(left)-lipgloss.Width(right))), right)
}

func (theme visualTheme) panel(title, body string, width int, active bool) string {
	style := theme.border
	if active {
		style = theme.active
	}
	innerWidth := max(1, tuiContentWidth(width)-style.GetHorizontalFrameSize())
	heading := theme.title.Render(title)
	body = terminal.Wrap(body, innerWidth)
	return style.Width(innerWidth).Render(heading + "\n\n" + body)
}

func (theme visualTheme) stepper(active int, width int) string {
	labels := []string{"Environment", "Review", "Ready"}
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
	if lipgloss.Width(line) > tuiContentWidth(width) {
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

func (theme visualTheme) help(items ...string) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		key, description, found := strings.Cut(item, " ")
		if !found {
			parts = append(parts, theme.key.Render(item))
			continue
		}
		parts = append(parts, theme.key.Render(key)+" "+theme.muted.Render(description))
	}
	return strings.Join(parts, theme.muted.Render("   "))
}

func friendlyProjectName(root string) string {
	name := filepath.Base(root)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "this project"
	}
	return terminal.SanitizeLine(name)
}
