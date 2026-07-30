//go:build windows

package inventory

import (
	"fmt"
	"path/filepath"
	"strings"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"golang.org/x/sys/windows"
)

// volumes enumerates fixed/removable logical drives.
//
// UNTESTED ON WINDOWS until first real CI/VM run — verify:
//   - C: appears with a stable volume serial id
//   - network drives are excluded
//   - empty CD drives do not panic
func volumes() []*breakwaterv1.VolumeInfo {
	var out []*breakwaterv1.VolumeInfo
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return []*breakwaterv1.VolumeInfo{{
			Id: "C", Mount: `C:\`, FsType: "unknown",
		}}
	}
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		root := letter + `:\`
		dtype := windows.GetDriveType(windows.StringToUTF16Ptr(root))
		switch dtype {
		case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE:
		default:
			continue
		}
		fsType := driveFS(root)
		size := driveSize(root)
		id := volumeSerial(root)
		if id == "" {
			id = letter
		}
		out = append(out, &breakwaterv1.VolumeInfo{
			Id:        id,
			Mount:     root,
			FsType:    fsType,
			SizeBytes: size,
		})
	}
	if len(out) == 0 {
		out = append(out, &breakwaterv1.VolumeInfo{
			Id: "C", Mount: `C:\`, FsType: "unknown",
		})
	}
	return out
}

func driveFS(root string) string {
	var fsName [windows.MAX_PATH + 1]uint16
	err := windows.GetVolumeInformation(
		windows.StringToUTF16Ptr(root),
		nil, 0, nil, nil, nil,
		&fsName[0], uint32(len(fsName)),
	)
	if err != nil {
		return "unknown"
	}
	return strings.ToLower(windows.UTF16ToString(fsName[:]))
}

func volumeSerial(root string) string {
	var serial uint32
	err := windows.GetVolumeInformation(
		windows.StringToUTF16Ptr(root),
		nil, 0, &serial, nil, nil, nil, 0,
	)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s-%08X", strings.TrimSuffix(root, `:\`), serial)
}

func driveSize(root string) int64 {
	var free, total, totalFree uint64
	path := filepath.Clean(root)
	err := windows.GetDiskFreeSpaceEx(
		windows.StringToUTF16Ptr(path),
		&free, &total, &totalFree,
	)
	if err != nil {
		return 0
	}
	return int64(total)
}
