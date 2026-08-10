// Package terminal contains the safety boundary between untrusted text and a
// user's terminal.
package terminal

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const (
	// DefaultSanitizedLimit bounds rendered untrusted text without silently
	// permitting a hostile value to consume an entire terminal or allocation.
	DefaultSanitizedLimit = 16 * 1024
	maskedSecret          = "********"
	truncationMarker      = "…"
)

// SanitizedBuilder incrementally escapes untrusted terminal text while
// enforcing one explicit bound across every appended section. It never emits
// a partial escape representation. Callers must reject the result when
// Complete reports false; unlike SanitizeN, this type never marks a truncated
// value as reviewable.
type SanitizedBuilder struct {
	output   strings.Builder
	limit    int
	exceeded bool
}

// NewSanitizedBuilder creates an incremental sanitizer with a total output
// byte limit. A non-positive limit rejects every non-empty append.
func NewSanitizedBuilder(limit int) *SanitizedBuilder {
	return &SanitizedBuilder{limit: limit}
}

// WriteString sanitizes and appends value. It returns false if the total
// sanitized output would exceed the configured bound.
func (builder *SanitizedBuilder) WriteString(value string) bool {
	if builder == nil || builder.exceeded {
		return false
	}
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		var replacement string
		if r == utf8.RuneError && size == 1 {
			replacement = fmt.Sprintf(`\x%02x`, value[0])
		} else {
			replacement = safeRune(r)
		}
		if builder.output.Len()+len(replacement) > builder.limit {
			builder.exceeded = true
			return false
		}
		builder.output.WriteString(replacement)
		value = value[size:]
	}
	return true
}

// Complete reports whether every appended byte is represented in String.
func (builder *SanitizedBuilder) Complete() bool {
	return builder != nil && !builder.exceeded
}

// String returns the safely escaped prefix accumulated so far. Callers must
// check Complete before treating it as a complete representation.
func (builder *SanitizedBuilder) String() string {
	if builder == nil {
		return ""
	}
	return builder.output.String()
}

// Sanitize converts terminal controls, invalid UTF-8, bidi controls, and other
// nonprinting Unicode into visible ASCII escapes. Newlines are retained; all
// other terminal-positioning controls, including tabs and carriage returns,
// are escaped. Output is bounded by DefaultSanitizedLimit bytes.
func Sanitize(value string) string {
	return SanitizeN(value, DefaultSanitizedLimit)
}

// SanitizeN is Sanitize with an explicit output-byte bound. It never splits a
// UTF-8 encoding or an escape representation. A non-positive limit emits an
// empty string.
func SanitizeN(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}

	var output strings.Builder
	output.Grow(min(len(value), limit))
	truncated := false
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		var replacement string
		if r == utf8.RuneError && size == 1 {
			replacement = fmt.Sprintf(`\x%02x`, value[0])
		} else {
			replacement = safeRune(r)
		}
		if output.Len()+len(replacement) > limit {
			truncated = true
			break
		}
		output.WriteString(replacement)
		value = value[size:]
	}
	if truncated {
		appendWithin(&output, truncationMarker, limit)
	}
	return output.String()
}

// SanitizeLine is suitable for a dynamic field embedded in a single terminal
// line. Newlines are represented visibly rather than becoming terminal input.
func SanitizeLine(value string) string {
	return strings.ReplaceAll(Sanitize(value), "\n", `\n`)
}

func safeRune(r rune) string {
	switch r {
	case '\n':
		return "\n"
	case '\a':
		return `\a`
	case '\b':
		return `\b`
	case '\t':
		return `\t`
	case '\r':
		return `\r`
	case '\v':
		return `\v`
	case '\f':
		return `\f`
	case 0x1b:
		return `\x1b`
	case 0x7f:
		return `\x7f`
	}
	if r < 0x20 || r >= 0x80 && r <= 0x9f {
		return fmt.Sprintf(`\x%02x`, r)
	}
	if isBidiControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsControl(r) || !unicode.IsPrint(r) {
		if r <= 0xffff {
			return fmt.Sprintf(`\u%04x`, r)
		}
		return fmt.Sprintf(`\U%08x`, r)
	}
	return string(r)
}

func isBidiControl(r rune) bool {
	return r == 0x061c || r == 0x200e || r == 0x200f ||
		r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

func appendWithin(output *strings.Builder, suffix string, limit int) {
	if len(suffix) <= limit-output.Len() {
		output.WriteString(suffix)
		return
	}
	for output.Len() > 0 && len(suffix) > limit-output.Len() {
		current := output.String()
		_, size := utf8.DecodeLastRuneInString(current)
		trimmed := current[:len(current)-size]
		output.Reset()
		output.WriteString(trimmed)
	}
	if len(suffix) <= limit-output.Len() {
		output.WriteString(suffix)
	}
}

// MaskSecret returns a fixed-width mask and never reveals whether a secret is
// empty or its underlying length. This is suitable for both TUI and accessible
// plain-text rendering.
func MaskSecret(string) string { return maskedSecret }

// ColorEnabled reports whether terminal styling is permitted by the process
// environment. The presence of NO_COLOR disables color even when its value is
// empty; TERM=dumb also disables it.
func ColorEnabled() bool {
	return ColorEnabledFor(os.LookupEnv)
}

// ColorEnabledFor provides an injectable environment lookup for adapters and
// focused tests.
func ColorEnabledFor(lookup func(string) (string, bool)) bool {
	if lookup == nil {
		return true
	}
	if _, present := lookup("NO_COLOR"); present {
		return false
	}
	term, _ := lookup("TERM")
	return !strings.EqualFold(strings.TrimSpace(term), "dumb")
}

// SGR applies an SGR parameter only when color is enabled. Dynamic text is
// sanitized before it is returned.
func SGR(parameter, value string, color bool) string {
	value = Sanitize(value)
	if !color || parameter == "" {
		return value
	}
	return "\x1b[" + parameter + "m" + value + "\x1b[0m"
}

// Width returns the terminal cell width of safe display text.
func Width(value string) int { return ansi.StringWidth(value) }

// Truncate shortens safe display text to width cells. It retains a visible
// marker where space permits and never splits a grapheme cluster.
func Truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(value) <= width {
		return value
	}
	if width == 1 {
		return truncationMarker
	}
	return ansi.Truncate(value, width, truncationMarker)
}

// Wrap hard-wraps safe display text at width cells while preserving existing
// newlines. It never emits a line wider than width.
func Wrap(value string, width int) string {
	if width <= 0 || value == "" {
		return ""
	}
	parts := strings.Split(value, "\n")
	for i, part := range parts {
		parts[i] = ansi.Hardwrap(part, width, true)
	}
	return strings.Join(parts, "\n")
}
