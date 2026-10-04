package content

import (
	"errors"
	"io"
	"sync"
)

// LimitReader confines both reads and seeks to a declared file, even when the
// underlying transport reader exposes a larger contiguous torrent address space.
func LimitReader(reader Reader, size int64) Reader { return &boundedReader{reader: reader, size: size} }

type boundedReader struct {
	reader         Reader
	position, size int64
	once           sync.Once
	closeErr       error
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.position >= r.size {
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-r.position {
		p = p[:r.size-r.position]
	}
	n, err := r.reader.Read(p)
	r.position += int64(n)
	return n, err
}
func (r *boundedReader) Seek(offset int64, whence int) (int64, error) {
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.position
	case io.SeekEnd:
		base = r.size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if offset < -base || offset > r.size-base {
		return 0, errors.New("seek outside content file")
	}
	position, err := r.reader.Seek(base+offset, io.SeekStart)
	if err == nil {
		r.position = position
	}
	return position, err
}
func (r *boundedReader) Close() error {
	r.once.Do(func() { r.closeErr = r.reader.Close() })
	return r.closeErr
}
