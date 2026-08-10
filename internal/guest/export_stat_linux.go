package guest

import "golang.org/x/sys/unix"

func exportMetadataTimeChanged(before, after unix.Stat_t) bool {
	return before.Ctim != after.Ctim
}
