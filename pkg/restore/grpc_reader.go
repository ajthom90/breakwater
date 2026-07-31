package restore

import (
	"context"
	"fmt"
	"io"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
)

// GRPCReader implements ObjectReader over RestoreService.GetObject.
type GRPCReader struct {
	Client breakwaterv1.RestoreServiceClient
}

// OpenObject streams the full object via GetObject.
func (g *GRPCReader) OpenObject(ctx context.Context, objectID string) (io.ReadCloser, error) {
	if g == nil || g.Client == nil {
		return nil, fmt.Errorf("restore: nil RestoreService client")
	}
	stream, err := g.Client.GetObject(ctx, &breakwaterv1.GetObjectRequest{ObjectId: objectID})
	if err != nil {
		return nil, err
	}
	return &objectStream{stream: stream}, nil
}

type objectStream struct {
	stream breakwaterv1.RestoreService_GetObjectClient
	buf    []byte
	err    error
}

func (s *objectStream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	for len(s.buf) == 0 {
		resp, err := s.stream.Recv()
		if err != nil {
			s.err = err
			return 0, err
		}
		s.buf = resp.GetData()
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *objectStream) Close() error {
	// Drain is optional; cancel context on the caller side ends the stream.
	s.err = io.EOF
	return nil
}
