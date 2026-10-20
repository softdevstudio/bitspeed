//go:build !linux && !darwin

package transfer

func receiverFilesystem(string) error { return ErrUnsupported }
