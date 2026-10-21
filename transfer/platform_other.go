//go:build !linux && !darwin && !windows

package transfer

import "os"

func spoolSupported() bool                   { return false }
func lockSpool(*os.File) error               { return ErrUnsupported }
func freeSpace(string) (int64, error)        { return 0, ErrUnsupported }
func openSource(string) (*os.File, error)    { return nil, ErrUnsupported }
func receiverFilesystem(string) error        { return ErrUnsupported }
func makePrivateDir(string) error            { return ErrUnsupported }
func privateDirOK(string, os.FileInfo) bool  { return false }
func privateFileOK(string, os.FileInfo) bool { return false }
func lockFileOK(string, os.FileInfo) bool    { return false }
func sealFile(*os.File) error                { return ErrUnsupported }
func sealedOK(string, os.FileInfo) bool      { return false }
func syncHandle(*os.File) error              { return ErrUnsupported }
func renamePath(string, string) error        { return ErrUnsupported }
