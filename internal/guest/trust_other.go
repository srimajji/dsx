//go:build !linux

package guest

const InstalledExecutable = "/usr/local/libexec/dsx/dsx-guest"

func VerifyInstalledExecutable() error { return nil }
