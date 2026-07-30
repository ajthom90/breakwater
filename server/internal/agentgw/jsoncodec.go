package agentgw

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

// jsonCodec is used for hand-written M1 RPCs until protoc generation is wired
// into the regular build. Name "json" → content-type application/grpc+json.
type jsonCodec struct{}

func (jsonCodec) Name() string                       { return "json" }
func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
