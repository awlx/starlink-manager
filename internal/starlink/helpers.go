package starlink

import (
	"bytes"
	"io"
)

// DeviceKind distinguishes user terminals (dishes) from routers.
type DeviceKind string

const (
	KindDish   DeviceKind = "dish"
	KindRouter DeviceKind = "router"
)

func bytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}

func ioReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, 16<<20))
}
