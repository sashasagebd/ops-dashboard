package host

import "syscall"

// diskUsage reports the filesystem containing path, computed the way `df`
// does. Blocks reserved for root (usually 5% on ext4) are in neither Used
// nor Available, which is why Used + Available < Total.
func diskUsage(path string) (Disk, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{}, err
	}
	bs := uint64(st.Bsize)
	return Disk{
		Total:     st.Blocks * bs,
		Used:      (st.Blocks - st.Bfree) * bs,
		Available: st.Bavail * bs,
	}, nil
}
