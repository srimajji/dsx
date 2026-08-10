//go:build linux

package guest

import (
	"errors"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func startOrphanReaper(done <-chan struct{}, directChildren func() map[int]struct{}) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	go func() {
		defer signal.Stop(signals)
		for {
			select {
			case <-done:
				managedDirectChildrenMu.Lock()
				reapAdoptedChildren(directChildren())
				managedDirectChildrenMu.Unlock()
				return
			case <-signals:
				managedDirectChildrenMu.Lock()
				reapAdoptedChildren(directChildren())
				managedDirectChildrenMu.Unlock()
			}
		}
	}()
}

func reapAdoptedChildren(direct map[int]struct{}) {
	for _, pid := range adoptedChildPIDs(direct) {
		_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
	}
}

func terminateAdoptedChildren(directChildren func() map[int]struct{}) {
	deadline := time.Now().Add(time.Second)
	for {
		managedDirectChildrenMu.Lock()
		pids := adoptedChildPIDs(directChildren())
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		}
		managedDirectChildrenMu.Unlock()
		if len(pids) == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func adoptedChildPIDs(direct map[int]struct{}) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	parent := os.Getpid()
	pids := make([]int, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if _, managed := direct[pid]; managed {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		closeParen := strings.LastIndexByte(string(stat), ')')
		if closeParen < 0 {
			continue
		}
		fields := strings.Fields(string(stat[closeParen+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err == nil && ppid == parent {
			pids = append(pids, pid)
		}
	}
	return pids
}

func reapOrphans() {
	deadline := time.Now().Add(time.Second)
	for {
		pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
		if pid > 0 {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			return
		}
		if err != nil || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
