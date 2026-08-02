package dockerengine

import "bytes"

// BoundedWriter consumes an untrusted stream while retaining only a bounded
// prefix for diagnostics or worker-protocol decoding.
type BoundedWriter struct {
	buffer   bytes.Buffer
	limit    int64
	overflow bool
}

func NewBoundedWriter(limit int64) *BoundedWriter { return &BoundedWriter{limit: limit} }

func (b *BoundedWriter) Write(value []byte) (int, error) {
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		count := int64(len(value))
		if count > remaining {
			count = remaining
		}
		_, _ = b.buffer.Write(value[:count])
	}
	if int64(len(value)) > remaining {
		b.overflow = true
	}
	return len(value), nil
}

func (b *BoundedWriter) Bytes() []byte  { return b.buffer.Bytes() }
func (b *BoundedWriter) Overflow() bool { return b.overflow }
