//go:build !windows

package inventory

import (
	"os"
	"path/filepath"
	"runtime"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
)

// volumes reports best-effort filesystem roots on non-Windows (dev/CI).
func volumes() []*breakwaterv1.VolumeInfo {
	var out []*breakwaterv1.VolumeInfo
	// Root of the current volume containing the working directory.
	wd, err := os.Getwd()
	if err != nil {
		wd = "/"
	}
	root := filepath.VolumeName(wd)
	if root == "" {
		root = "/"
	}
	// Also include home if different.
	out = append(out, &breakwaterv1.VolumeInfo{
		Id:        "root",
		Mount:     root,
		FsType:    runtime.GOOS + "-fs",
		SizeBytes: 0,
	})
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != root {
		out = append(out, &breakwaterv1.VolumeInfo{
			Id:     "home",
			Mount:  home,
			FsType: runtime.GOOS + "-fs",
		})
	}
	return out
}
