//go:build !linux && !darwin

package transfer

import "os"

func spoolSupported() bool            { return false }
func lockSpool(*os.File) error        { return ErrUnsupported }
func freeSpace(string) (int64, error) { return 0, ErrUnsupported }

func openSource(string) (*os.File, error) { return nil, ErrUnsupported }
