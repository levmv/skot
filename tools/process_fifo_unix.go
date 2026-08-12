//go:build darwin || linux

package tools

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// createJobControl creates the per-job FIFO and opens its read side before the
// worker is forked. The descriptor is CLOEXEC in the manager; os/exec clears
// that flag only on the copy explicitly passed through ExtraFiles.
func createJobControl(path string) (*os.File, error) {
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, fmt.Errorf("create job control FIFO: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("restrict job control FIFO: %w", err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open job control FIFO reader: %w", err)
	}
	reader := os.NewFile(uintptr(fd), path)
	if reader == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open job control FIFO reader: invalid descriptor")
	}
	if err := validateJobControlFile(reader, path); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return reader, nil
}

// inheritJobControl takes ownership of the fixed descriptor installed by
// ExtraFiles. A private dummy writer keeps blocking reads from seeing EOF when
// no manager currently has the FIFO open. Both descriptors are explicitly
// CLOEXEC so the payload cannot keep the worker's lifecycle lease alive.
func inheritJobControl(path string) (*os.File, *os.File, error) {
	reader := os.NewFile(uintptr(jobControlFD), path)
	if reader == nil {
		return nil, nil, errors.New("inherit job control FIFO: descriptor is unavailable")
	}
	closeReader := true
	defer func() {
		if closeReader {
			_ = reader.Close()
		}
	}()
	if err := validateJobControlFile(reader, path); err != nil {
		return nil, nil, fmt.Errorf("inherit job control FIFO: %w", err)
	}
	unix.CloseOnExec(int(reader.Fd()))

	dummy, err := openJobControlWriter(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open private job control writer: %w", err)
	}
	if err := unix.SetNonblock(int(reader.Fd()), false); err != nil {
		_ = dummy.Close()
		return nil, nil, fmt.Errorf("make job control FIFO blocking: %w", err)
	}
	closeReader = false
	return reader, dummy, nil
}

func validateJobControlFile(file *os.File, path string) error {
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat opened job control FIFO: %w", err)
	}
	named, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat job control FIFO: %w", err)
	}
	if opened.Mode()&os.ModeNamedPipe == 0 || named.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("job control path is not a FIFO")
	}
	if named.Mode().Perm() != 0o600 {
		return fmt.Errorf("job control FIFO mode is %04o, want 0600", named.Mode().Perm())
	}
	if !os.SameFile(opened, named) {
		return errors.New("job control descriptor does not match its path")
	}
	return nil
}

func openJobControlWriter(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	writer := os.NewFile(uintptr(fd), path)
	if writer == nil {
		_ = unix.Close(fd)
		return nil, errors.New("invalid job control writer descriptor")
	}
	if err := validateJobControlFile(writer, path); err != nil {
		_ = writer.Close()
		return nil, err
	}
	unix.CloseOnExec(int(writer.Fd()))
	return writer, nil
}

// probeJobControl asks the kernel whether the FIFO still has a reader. ENXIO
// is the precise absent-owner answer. Missing or malformed filesystem state is
// returned as an error rather than being turned into an invented lifecycle.
func probeJobControl(path string) (bool, error) {
	writer, err := openJobControlWriter(path)
	if errors.Is(err, unix.ENXIO) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, writer.Close()
}

func writeJobControl(path string, command byte) error {
	writer, err := openJobControlWriter(path)
	if err != nil {
		return err
	}
	defer writer.Close()
	return writeJobControlCommand(writer, command)
}

func writeJobControlCommand(writer *os.File, command byte) error {
	written, err := unix.Write(int(writer.Fd()), []byte{command})
	if err != nil {
		return err
	}
	if written != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func readJobControl(reader *os.File, stop chan<- struct{}) error {
	buffer := make([]byte, jobControlMaxRead)
	for {
		count, err := reader.Read(buffer)
		for _, command := range buffer[:count] {
			if command != jobControlStop {
				continue
			}
			select {
			case stop <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return err
		}
	}
}
