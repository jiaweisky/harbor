package blob

import (
	"context"
	"fmt"
	"io"
)

// part is one uploaded part of a multipart upload.
type part struct {
	number int
	etag   string
}

// multipartTarget is a backend's multipart-upload operations. uploadMultipart
// sequences them, so each backend supplies only its request signing.
type multipartTarget interface {
	begin(ctx context.Context) (id string, err error)
	putPart(ctx context.Context, id string, n int, data []byte) (etag string, err error)
	complete(ctx context.Context, id string, parts []part) error
	abort(ctx context.Context, id string)
}

// uploadMultipart streams size bytes from r to t, splitting them into partSize
// chunks. It aborts the upload when a part fails.
func uploadMultipart(ctx context.Context, t multipartTarget, partSize int, size int64, r io.Reader) error {
	id, err := t.begin(ctx)
	if err != nil {
		return err
	}
	buf := make([]byte, partSize)
	var parts []part
	for n := 1; size > 0; n++ {
		m := int64(partSize)
		if m > size {
			m = size
		}
		chunk := buf[:m]
		if _, err := io.ReadFull(r, chunk); err != nil {
			t.abort(ctx, id)
			return fmt.Errorf("blob: read multipart part %d: %w", n, err)
		}
		etag, err := t.putPart(ctx, id, n, chunk)
		if err != nil {
			t.abort(ctx, id)
			return err
		}
		parts = append(parts, part{n, etag})
		size -= m
	}
	return t.complete(ctx, id, parts)
}
