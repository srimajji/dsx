package terminal

import (
	"strings"
	"testing"
	"unicode"
)

func TestSanitizeHostileTerminalText(t *testing.T) {
	hostile := "safe\x1b[2J\x1b]0;owned\a\x1b]52;c;Y2xpcGJvYXJk\a\x1bPkeys\x1b\\\r\t\x00\u202espoof\u2066name\u2069\u200b café\nnext"
	got := Sanitize(hostile)
	for _, forbidden := range []string{"\x1b", "\a", "\r", "\t", "\x00", "\u202e", "\u2066", "\u2069", "\u200b"} {
		if strings.ContainsRune(got, []rune(forbidden)[0]) && len([]rune(forbidden)) == 1 {
			t.Fatalf("sanitized text retained control %q: %q", forbidden, got)
		}
	}
	for _, visible := range []string{`\x1b[2J`, `\x1b]0;owned\a`, `\x1b]52;c;Y2xpcGJvYXJk\a`, `\x1bPkeys\x1b\`, `\r`, `\t`, `\x00`, `\u202e`, `\u2066`, `\u2069`, `\u200b`, "café\nnext"} {
		if !strings.Contains(got, visible) {
			t.Errorf("Sanitize() = %q, missing %q", got, visible)
		}
	}
	for _, r := range got {
		if r != '\n' && (unicode.IsControl(r) || r == '\u202e') {
			t.Fatalf("Sanitize() retained nonprinting rune U+%04X", r)
		}
	}
}

func TestSanitizeBoundedAndInvalidUTF8(t *testing.T) {
	got := SanitizeN(string([]byte{'a', 0xff, 'b'})+strings.Repeat("x", 100), 12)
	if len(got) > 12 {
		t.Fatalf("SanitizeN output length = %d, want <= 12: %q", len(got), got)
	}
	if !strings.Contains(got, `\xff`) || !strings.HasSuffix(got, "…") {
		t.Fatalf("SanitizeN() = %q, want invalid byte escape and truncation marker", got)
	}
}

func TestSanitizeIncrementalReviewDoesNotTruncateTail(t *testing.T) {
	const tail = "tail-trust-grant"
	builder := NewSanitizedBuilder(64 * 1024)
	if !builder.WriteString(strings.Repeat("grant\n", 4*1024)) || !builder.WriteString("\x1b[31m"+tail) || !builder.Complete() {
		t.Fatal("complete incremental review was rejected")
	}
	got := builder.String()
	if len(got) <= DefaultSanitizedLimit || !strings.HasSuffix(got, `\x1b[31m`+tail) {
		t.Fatalf("incremental review lost tail grant: length=%d suffix=%q", len(got), got[max(0, len(got)-64):])
	}

	refused := NewSanitizedBuilder(32)
	if refused.WriteString(strings.Repeat("x", 33)) || refused.Complete() {
		t.Fatal("over-bound incremental review was marked complete")
	}
}

func TestSanitizeWidthAwareLayout(t *testing.T) {
	for _, width := range []int{20, 40, 80, 120} {
		wrapped := Wrap(Sanitize("界面 "+strings.Repeat("word ", 80)), width)
		for _, line := range strings.Split(wrapped, "\n") {
			if got := Width(line); got > width {
				t.Fatalf("width %d rendered line width %d: %q", width, got, line)
			}
		}
		truncated := Truncate(Sanitize(strings.Repeat("界", width)), width)
		if got := Width(truncated); got > width {
			t.Fatalf("Truncate width = %d, want <= %d: %q", got, width, truncated)
		}
	}
}

func TestNoColorPolicyAndMask(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}
	}
	if ColorEnabledFor(lookup(map[string]string{"NO_COLOR": ""})) {
		t.Fatal("NO_COLOR presence enabled color")
	}
	if ColorEnabledFor(lookup(map[string]string{"TERM": "dumb"})) {
		t.Fatal("TERM=dumb enabled color")
	}
	if !ColorEnabledFor(lookup(map[string]string{"TERM": "xterm-256color"})) {
		t.Fatal("capable terminal disabled color")
	}
	plain := SGR("31", "error\x1b]0;pwned\a", false)
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("disabled color emitted ESC: %q", plain)
	}
	if MaskSecret("") != MaskSecret("long-secret-value") || strings.Contains(MaskSecret("secret"), "secret") {
		t.Fatal("secret mask leaks emptiness, value, or length")
	}
}
