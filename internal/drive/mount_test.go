package drive

import (
	"errors"
	"testing"
)

func TestMountPointMac(t *testing.T) {
	status := " Vendor   Product           Rev \n PIONEER  BD-RW   BDR-XD07  1.00\n\n           Type: BD-R               Name: /dev/disk4\n"
	node, ok := parseDrutilDevNode(status)
	if !ok || node != "/dev/disk4" {
		t.Fatalf("device node = %q, %v", node, ok)
	}
	mounts := parseMount("/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)\n" +
		"/dev/disk41 on /Volumes/Other (cd9660, local)\n" +
		"/dev/disk4 on /Volumes/My Disc (on) (cd9660, local, nodev, nosuid, read-only)\n")
	got, err := mountedAt(mounts, node)
	if err != nil || got != "/Volumes/My Disc (on)" {
		t.Fatalf("mount point = %q, %v", got, err)
	}
}

func TestMountPointMacSlice(t *testing.T) {
	mounts := parseMount("/dev/disk41 on /Volumes/Other (cd9660, local)\n" +
		"/dev/disk4s1 on /Volumes/BACKUP_1 (udf, local, read-only)\n")
	got, err := mountedAt(mounts, "/dev/disk4")
	if err != nil || got != "/Volumes/BACKUP_1" {
		t.Fatalf("mount point = %q, %v", got, err)
	}
}

func TestMountPointLinux(t *testing.T) {
	mounts := parseProcMounts("/dev/sda1 / ext4 rw 0 0\n" +
		"/dev/sr0 /media/ada/My\\040Disc iso9660 ro,nosuid 0 0\n")
	got, err := mountedAt(mounts, "/dev/sr0")
	if err != nil || got != "/media/ada/My Disc" {
		t.Fatalf("mount point = %q, %v", got, err)
	}
}

func TestMountPointNotMounted(t *testing.T) {
	if _, err := mountedAt(parseMount("/dev/disk3s1s1 on / (apfs)\n"), "/dev/disk4"); !errors.Is(err, ErrNotMounted) {
		t.Fatalf("err = %v, want ErrNotMounted", err)
	}
}
