// Package hasher computes xxHash64 checksums of files using bounded memory.
package hasher

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/cespare/xxhash/v2"
)

// bufferSize is the size of the pooled copy buffer. A larger buffer than the
// io.Copy default (32 KiB) reduces the number of read syscalls on big files
// while keeping memory usage constant regardless of file size.
const bufferSize = 256 * 1024

var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, bufferSize)
		return &b
	},
}

// ctxReader aborts reading as soon as its context is cancelled, so a signal
// can interrupt the hashing of a multi-gigabyte file promptly.
//
// It deliberately exposes only Read: hiding optional interfaces such as
// io.WriterTo (implemented by *os.File) guarantees that io.CopyBuffer really
// uses the pooled buffer.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// File streams the file at path through xxHash64 (seed 0) and returns the sum
// and the number of bytes hashed. The file is never loaded into memory as a
// whole.
func File(ctx context.Context, path string) (sum uint64, n int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("hash %q: %w", path, err)
	}
	defer f.Close()

	sum, n, err = Reader(ctx, f)
	if err != nil {
		return 0, n, fmt.Errorf("hash %q: %w", path, err)
	}
	return sum, n, nil
}

// Reader streams r through xxHash64 (seed 0) and returns the sum and the
// number of bytes consumed.
func Reader(ctx context.Context, r io.Reader) (sum uint64, n int64, err error) {
	h := xxhash.New()

	bp := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bp)

	n, err = io.CopyBuffer(h, ctxReader{ctx: ctx, r: r}, *bp)
	if err != nil {
		return 0, n, err
	}
	return h.Sum64(), n, nil
}
