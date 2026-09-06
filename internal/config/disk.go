// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package config

import "syscall"

// diskSize reports the size in bytes of the file system holding path. It
// is a variable so a test covers the failure branch of Resolve.
var diskSize = func(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Blocks) * int64(st.Bsize), nil //nolint:unconvert // Bsize is int64 on darwin and int64/uint32 elsewhere
}
