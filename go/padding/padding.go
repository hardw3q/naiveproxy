// Package padding implements the NaiveProxy padding protocol.
// Ported directly from github.com/klzgrad/forwardproxy (forwardproxy.go).
// The padding format is identical, ensuring wire compatibility with the server.
package padding

import (
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
)

const (
	NoPadding        = 0
	AddPadding       = 1
	RemovePadding    = 2
	NumFirstPaddings = 8
)

var bufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 64*1024)
		return &buf
	},
}

type closeWriter interface {
	CloseWrite() error
}

// DualStream copies bidirectionally between target and the client reader/writer,
// applying the NaiveProxy padding protocol when padding=true.
// Identical logic to forwardproxy's dualStream.
func DualStream(target net.Conn, clientReader io.ReadCloser, clientWriter io.Writer, padding bool) error {
	stream := func(w io.Writer, r io.Reader, paddingType int) error {
		bufPtr := bufferPool.Get().(*[]byte)
		buf := *bufPtr
		buf = buf[0:cap(buf)]
		_, err := FlushingIoCopy(w, r, buf, paddingType)
		bufferPool.Put(bufPtr)
		if cw, ok := w.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		return err
	}
	if padding {
		go stream(target, clientReader, RemovePadding) //nolint:errcheck
		return stream(clientWriter, target, AddPadding)
	}
	go stream(target, clientReader, NoPadding) //nolint:errcheck
	return stream(clientWriter, target, NoPadding)
}

// FlushingIoCopy copies src→dst, applying padding encoding/decoding for the
// first NumFirstPaddings (8) iterations. Flushes on each write when dst is
// an http.ResponseWriter. Identical to forwardproxy's flushingIoCopy.
func FlushingIoCopy(dst io.Writer, src io.Reader, buf []byte, paddingType int) (written int64, err error) {
	rw, ok := dst.(http.ResponseWriter)
	var rc *http.ResponseController
	if ok {
		rc = http.NewResponseController(rw)
	}
	var numPadding int
	for {
		var nr int
		var er error
		if paddingType == AddPadding && numPadding < NumFirstPaddings {
			numPadding++
			paddingSize := rand.Intn(256)
			maxRead := 65536 - 3 - paddingSize
			nr, er = src.Read(buf[3:maxRead])
			if nr > 0 {
				buf[0] = byte(nr / 256)
				buf[1] = byte(nr % 256)
				buf[2] = byte(paddingSize)
				for i := 0; i < paddingSize; i++ {
					buf[3+nr+i] = 0
				}
				nr += 3 + paddingSize
			}
		} else if paddingType == RemovePadding && numPadding < NumFirstPaddings {
			numPadding++
			nr, er = io.ReadFull(src, buf[0:3])
			if nr > 0 {
				nr = int(buf[0])*256 + int(buf[1])
				paddingSize := int(buf[2])
				nr, er = io.ReadFull(src, buf[0:nr])
				if nr > 0 {
					var junk [256]byte
					_, er = io.ReadFull(src, junk[0:paddingSize])
				}
			}
		} else {
			nr, er = src.Read(buf)
		}
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if rc != nil {
				if ef := rc.Flush(); ef != nil {
					err = ef
					break
				}
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return
}
