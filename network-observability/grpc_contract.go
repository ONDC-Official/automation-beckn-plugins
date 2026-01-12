package networkobservability

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// grpcContract defines how the plugin turns a JSON payload into a protobuf request
// and which protobuf response type to expect.
//
// Default behavior uses google.protobuf.BytesValue -> google.protobuf.Empty and
// invokes the configured method (or audit default).
//
// Teams can rebuild the plugin for their own .proto by calling SetGRPCContract
// from an init() function in an additional file compiled into the same package.
type grpcContract interface {
	FullMethod(configured string) string
	NewRequest() proto.Message
	NewResponse() proto.Message
	FillRequest(req proto.Message, body []byte) error
}

type defaultGRPCContract struct{}

func (defaultGRPCContract) FullMethod(configured string) string {
	fullMethod := strings.TrimSpace(configured)
	if fullMethod == "" {
		fullMethod = "/beckn.audit.v1.AuditService/LogEvent"
	}
	return fullMethod
}

func (defaultGRPCContract) NewRequest() proto.Message {
	return &wrapperspb.BytesValue{}
}

func (defaultGRPCContract) NewResponse() proto.Message {
	return &emptypb.Empty{}
}

func (defaultGRPCContract) FillRequest(req proto.Message, body []byte) error {
	b, ok := req.(*wrapperspb.BytesValue)
	if !ok {
		return fmt.Errorf("network-observability: default grpc contract expected *wrapperspb.BytesValue, got %T", req)
	}
	b.Value = body
	return nil
}

var activeGRPCContract grpcContract = defaultGRPCContract{}

// SetGRPCContract allows a build-time customization of how gRPC request/response
// protobuf messages are constructed. This is intended for teams that want to use
// their own generated protobuf types (their own .proto) while keeping the plugin
// core code unchanged.
//
// Call this from an init() function in another file that is compiled into the
// same package.
func SetGRPCContract(c grpcContract) {
	if c == nil {
		activeGRPCContract = defaultGRPCContract{}
		return
	}
	activeGRPCContract = c
}
