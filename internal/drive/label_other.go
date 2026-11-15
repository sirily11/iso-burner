//go:build !windows

package drive

func windowsVolumeLabel(string) string { return "" }
