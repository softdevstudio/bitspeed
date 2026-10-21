//go:build windows

package transfer

import (
	"errors"
	"math"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// See docs/windows-persistence.md for how each call maps to the Unix sequence.

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procLockFileEx           = kernel32.NewProc("LockFileEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumePathNameW   = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeInformation = kernel32.NewProc("GetVolumeInformationW")
	procGetDriveTypeW        = kernel32.NewProc("GetDriveTypeW")
	procMoveFileExW          = kernel32.NewProc("MoveFileExW")
	procLocalFree            = kernel32.NewProc("LocalFree")
	procGetSecurityInfo      = advapi32.NewProc("GetSecurityInfo")
	procGetAce               = advapi32.NewProc("GetAce")
	procStringToSD           = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

const (
	lockfileFailImmediately  = 0x1
	lockfileExclusiveLock    = 0x2
	driveFixed               = 3
	fileSupportsHardLinks    = 0x00400000
	movefileReplaceExisting  = 0x1
	movefileWriteThrough     = 0x8
	readControl              = 0x00020000
	fileWriteData            = 0x0002
	fileAppendData           = 0x0004
	fileFlagSequentialScan   = 0x08000000
	seFileObject             = 1
	ownerSecurityInformation = 0x1
	daclSecurityInformation  = 0x4
	accessAllowedACEType     = 0
	accessDeniedACEType      = 1
	errorInvalidFunction     = syscall.Errno(1)
	shareAll                 = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE
)

func spoolSupported() bool { return true }

func lockSpool(f *os.File) error {
	var overlapped syscall.Overlapped
	r, _, e := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r == 0 {
		return e
	}
	return nil
}

func freeSpace(path string) (int64, error) {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return 0, e
	}
	var available, total, free uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&available)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, e
	}
	if available > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}

// The receiver accepts local NTFS on fixed drives, where hard links, write-through
// renames and directory flushes are available.
func receiverFilesystem(path string) error {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return ErrStorage
	}
	volume := make([]uint16, syscall.MAX_PATH+1)
	if r, _, _ := procGetVolumePathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&volume[0])), uintptr(len(volume))); r == 0 {
		return ErrStorage
	}
	if r, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(&volume[0]))); r != driveFixed {
		return ErrUnsupported
	}
	var flags uint32
	name := make([]uint16, syscall.MAX_PATH+1)
	if r, _, _ := procGetVolumeInformation.Call(uintptr(unsafe.Pointer(&volume[0])), 0, 0, 0, 0, uintptr(unsafe.Pointer(&flags)), uintptr(unsafe.Pointer(&name[0])), uintptr(len(name))); r == 0 {
		return ErrStorage
	}
	if syscall.UTF16ToString(name) != "NTFS" || flags&fileSupportsHardLinks == 0 {
		return ErrUnsupported
	}
	return nil
}

// Sharing only reads refuses the open while another process can write the
// source, and blocks writers, renames and deletes until the copy is closed.
func openSource(path string) (*os.File, error) {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT|fileFlagSequentialScan, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(h), path), nil
}

var (
	identityOnce sync.Once
	identitySID  string
	identityErr  error
)

func currentSID() (string, error) {
	identityOnce.Do(func() {
		token, e := syscall.OpenCurrentProcessToken()
		if e != nil {
			identityErr = e
			return
		}
		defer token.Close()
		user, e := token.GetTokenUser()
		if e != nil {
			identityErr = e
			return
		}
		identitySID, identityErr = user.User.Sid.String()
	})
	return identitySID, identityErr
}

// Only these accounts may hold allow entries on private files and directories.
func allowedSID(sid string) bool {
	user, e := currentSID()
	return e == nil && (sid == user || sid == "S-1-5-18" || sid == "S-1-5-32-544")
}

// The protected DACL is part of the create call, so the directory never exists
// with inherited access for other accounts. Children inherit it.
func makePrivateDir(path string) error {
	user, e := currentSID()
	if e != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: e}
	}
	sddl, e := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;" + user + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if e != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: e}
	}
	var sd uintptr
	if r, _, e := procStringToSD.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return &os.PathError{Op: "mkdir", Path: path, Err: e}
	}
	defer procLocalFree.Call(sd)
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: e}
	}
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: sd}
	if e = syscall.CreateDirectory(p, &sa); e != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: e}
	}
	return nil
}

type winACL struct {
	revision, sbz1 byte
	size, count, _ uint16
}
type winACE struct {
	kind, flags byte
	size        uint16
	mask        uint32
}

// privateObject reads the DACL through a handle that does not follow reparse
// points. The owner must be an allowed account, and the DACL may hold only allow
// entries for allowed accounts or OWNER RIGHTS, plus any deny entries.
func privateObject(path string, wantDir bool) bool {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return false
	}
	h, e := syscall.CreateFile(p, readControl, shareAll, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &info) != nil {
		return false
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != wantDir {
		return false
	}
	var owner *syscall.SID
	var dacl *winACL
	var sd uintptr
	if r, _, _ := procGetSecurityInfo.Call(uintptr(h), seFileObject, ownerSecurityInformation|daclSecurityInformation, uintptr(unsafe.Pointer(&owner)), 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd))); r != 0 {
		return false
	}
	defer procLocalFree.Call(sd)
	// An owner outside the allowed set could open the object up at any time.
	if owner == nil || dacl == nil {
		return false
	}
	if sid, e := owner.String(); e != nil || !allowedSID(sid) {
		return false
	}
	for i := uint16(0); i < dacl.count; i++ {
		var ace *winACE
		if r, _, _ := procGetAce.Call(uintptr(unsafe.Pointer(dacl)), uintptr(i), uintptr(unsafe.Pointer(&ace))); r == 0 || ace == nil {
			return false
		}
		switch ace.kind {
		case accessDeniedACEType:
		case accessAllowedACEType:
			sid, e := (*syscall.SID)(unsafe.Add(unsafe.Pointer(ace), 8)).String()
			// OWNER RIGHTS (Python's private directories use it) means the owner checked above.
			if e != nil || !allowedSID(sid) && sid != "S-1-3-4" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func privateDirOK(path string, info os.FileInfo) bool {
	return info.IsDir() && privateObject(path, true)
}
func privateFileOK(path string, info os.FileInfo) bool {
	return info.Mode().IsRegular() && privateObject(path, false)
}
func lockFileOK(path string, info os.FileInfo) bool { return privateFileOK(path, info) }

// The read-only attribute would make later flushes impossible, so sealed files
// are ordinary private files on Windows. Every reuse revalidates their SHA-256.
func sealFile(*os.File) error                     { return nil }
func sealedOK(path string, info os.FileInfo) bool { return privateFileOK(path, info) }

// FlushFileBuffers needs write access, which read handles and directory handles
// opened by os.Open lack. Flush a second handle to the same object instead; the
// flush covers the object's cached data whichever handle wrote it.
func syncHandle(f *os.File) error {
	e := f.Sync()
	if e == nil || !errors.Is(e, syscall.ERROR_ACCESS_DENIED) && !errors.Is(e, errorInvalidFunction) {
		return e
	}
	return flushPath(f)
}

func flushPath(f *os.File) error {
	p, e := syscall.UTF16PtrFromString(f.Name())
	if e != nil {
		return e
	}
	var h syscall.Handle
	for _, access := range []uint32{fileWriteData, fileAppendData} {
		h, e = syscall.CreateFile(p, access, shareAll, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if e == nil || !errors.Is(e, syscall.ERROR_ACCESS_DENIED) {
			break
		}
	}
	if e != nil {
		return &os.PathError{Op: "flush", Path: f.Name(), Err: e}
	}
	defer syscall.CloseHandle(h)
	var a, b syscall.ByHandleFileInformation
	if e = syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &a); e != nil {
		return e
	}
	if e = syscall.GetFileInformationByHandle(h, &b); e != nil {
		return e
	}
	if a.VolumeSerialNumber != b.VolumeSerialNumber || a.FileIndexHigh != b.FileIndexHigh || a.FileIndexLow != b.FileIndexLow {
		return &os.PathError{Op: "flush", Path: f.Name(), Err: errors.New("path no longer names the open file")}
	}
	if e = syscall.FlushFileBuffers(h); e != nil {
		return &os.PathError{Op: "flush", Path: f.Name(), Err: e}
	}
	return nil
}

func renamePath(a, b string) error {
	from, e := syscall.UTF16PtrFromString(a)
	if e != nil {
		return e
	}
	to, e := syscall.UTF16PtrFromString(b)
	if e != nil {
		return e
	}
	if r, _, e := procMoveFileExW.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), movefileReplaceExisting|movefileWriteThrough); r == 0 {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: e}
	}
	return nil
}
