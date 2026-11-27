package sandbox

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Reading and writing in directories that agents can write too (container
// homes, repositories): always through an os.Root of the directory, never
// through a symlink at the end of the path, never from a FIFO or device,
// and never more than the caller can hold.

// openRegular opens name in root for reading, refusing anything but a
// regular file.
func openRegular(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	return openRegularFlags(root, name, syscall.O_NOFOLLOW)
}

func openRegularFlags(root *os.Root, name string, flags int) (*os.File, os.FileInfo, error) {
	// O_NONBLOCK: a FIFO swapped in since a Stat must not hang the open.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|flags, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", name)
	}
	return f, fi, nil
}

// readRegular reads name in root, up to max bytes (a larger file is an
// error, whatever its size claims: sparse files cost nothing to make).
func readRegular(root *os.Root, name string, max int64) ([]byte, error) {
	return readRegularFlags(root, name, max, syscall.O_NOFOLLOW)
}

// readInRoot is readRegular for a file that may be a symlink to another
// one in the same root (a repository's own links), which os.Root keeps it
// to.
func readInRoot(root *os.Root, name string, max int64) ([]byte, error) {
	return readRegularFlags(root, name, max, 0)
}

func readRegularFlags(root *os.Root, name string, max int64, flags int) ([]byte, error) {
	f, fi, err := openRegularFlags(root, name, flags)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, max)
	}
	return b, nil
}

// writeInto writes what r holds to name in root: into a new temporary
// file, renamed over name, so neither a planted symlink at name nor at the
// temporary name is followed.
func writeInto(root *os.Root, name string, r io.Reader) error {
	if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp := name + ".collie-tmp"
	_ = root.Remove(tmp)
	out, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, name)
}

// writeBytesInto is writeInto for data in memory.
func writeBytesInto(root *os.Root, name string, b []byte) error {
	return writeInto(root, name, bytes.NewReader(b))
}

// cappedBuffer keeps at most max bytes of what is written to it and then
// calls full (once), e.g. to stop the process writing. Writes never fail,
// so the writer isn't told to stop by an error first. The bytes.Buffer is
// a field, not embedded: its ReadFrom would be promoted, and io.Copy (as
// os/exec uses it) would take that and skip Write and the cap.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
	full func()
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.over {
		return len(p), nil
	}
	if room := b.max - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		b.over = true
		if b.full != nil {
			b.full()
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }
