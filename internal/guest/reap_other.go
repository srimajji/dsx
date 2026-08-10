//go:build !linux

package guest

func reapOrphans() {}

func startOrphanReaper(<-chan struct{}, func() map[int]struct{}) {}

func terminateAdoptedChildren(func() map[int]struct{}) {}
