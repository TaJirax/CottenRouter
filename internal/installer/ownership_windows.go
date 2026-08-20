//go:build windows

package installer

func preserveFileOwnership(_, _ string) error { return nil }
