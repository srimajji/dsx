//go:build !linux

package guest

func lockProcessPrivileges() error { return nil }

func enableChildSubreaper() error { return nil }

func startWithoutPrivilegeGains(start func() error) error { return start() }
