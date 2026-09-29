//go:build !windows

package printer

func systemService() Service { return CUPS{Run: runCommand} }
