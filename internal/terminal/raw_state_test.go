//go:build darwin

package terminal

import (
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestRawStateMakesTerminalRawAndRestoresExactFlags(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()

	before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	state := NewRawState(slave)
	if err := state.Release(); err != nil {
		t.Fatal(err)
	}
	raw, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Lflag&(unix.ICANON|unix.ECHO) != 0 {
		t.Fatalf("raw lflag = %#x", raw.Lflag)
	}
	if err := state.Restore(); err != nil {
		t.Fatal(err)
	}
	after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatal(err)
	}
	if *after != *before {
		t.Fatalf("terminal state was not restored: before=%#v after=%#v", before, after)
	}
}
