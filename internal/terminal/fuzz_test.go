package terminal

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func FuzzTerminalSanitizer(f *testing.F) {
	for _, seed := range []struct {
		data  []byte
		limit int
	}{
		{[]byte("plain text"), DefaultSanitizedLimit},
		{[]byte("\x1b[2J\x1b]52;c;dG9rZW4=\a"), DefaultSanitizedLimit},
		{[]byte("line one\r\nline two\tend"), 32},
		{[]byte{0xff, 0xfe, 0x00, 0x7f, 'A'}, 64},
		{[]byte("left\u202eright\u2066hidden\u2069"), 128},
		{[]byte(strings.Repeat("\x1b", DefaultSanitizedLimit)), DefaultSanitizedLimit},
		{[]byte("truncated escape \x1b["), 1},
	} {
		f.Add(seed.data, seed.limit)
	}

	f.Fuzz(func(t *testing.T, data []byte, rawLimit int) {
		if len(data) > 64*1024 {
			t.Skip()
		}
		limit := rawLimit % (2*DefaultSanitizedLimit + 1)
		got := SanitizeN(string(data), limit)
		if got != SanitizeN(string(data), limit) {
			t.Fatal("SanitizeN is nondeterministic")
		}
		if limit <= 0 {
			if got != "" {
				t.Fatalf("SanitizeN with non-positive limit returned %q", got)
			}
			return
		}
		if len(got) > limit {
			t.Fatalf("SanitizeN output length %d exceeds limit %d", len(got), limit)
		}
		assertTerminalSafe(t, got, true)
		line := SanitizeLine(string(data))
		assertTerminalSafe(t, line, false)
	})
}

func assertTerminalSafe(t *testing.T, value string, allowNewline bool) {
	t.Helper()
	if !utf8.ValidString(value) {
		t.Fatalf("sanitizer emitted invalid UTF-8: %q", value)
	}
	for _, r := range value {
		if r == '\n' && allowNewline {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatalf("sanitizer leaked control rune U+%04X in %q", r, value)
		}
	}
}
