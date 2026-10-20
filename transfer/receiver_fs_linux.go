package transfer

import "syscall"

func receiverFilesystem(path string) error {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return ErrStorage
	}
	// Local ext, XFS, Btrfs and overlay mounts are the initial tested families.
	switch uint64(st.Type) {
	case 0xef53, 0x58465342, 0x9123683e, 0x794c7630:
		return nil
	}
	return ErrUnsupported
}
