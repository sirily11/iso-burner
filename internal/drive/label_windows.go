//go:build windows

package drive

import (
	"strings"

	"golang.org/x/sys/windows"
)

// windowsVolumeLabel asks the filesystem directly for an optical disc's label.
func windowsVolumeLabel(id string) string {
	letter := strings.TrimSuffix(id, ":")
	if len(letter) != 1 || !((letter[0] >= 'A' && letter[0] <= 'Z') || (letter[0] >= 'a' && letter[0] <= 'z')) {
		return ""
	}
	root, err := windows.UTF16PtrFromString(strings.ToUpper(letter) + `:\`)
	if err != nil {
		return ""
	}
	var label [256]uint16
	if err := windows.GetVolumeInformation(root, &label[0], uint32(len(label)), nil, nil, nil, nil, 0); err != nil {
		return ""
	}
	return strings.TrimSpace(windows.UTF16ToString(label[:]))
}
