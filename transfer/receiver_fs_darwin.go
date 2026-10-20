package transfer

import "syscall"

func receiverFilesystem(path string) error {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return ErrStorage
	}
	name := []byte{}
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	if string(name) == "apfs" || string(name) == "hfs" {
		return nil
	}
	return ErrUnsupported
}
