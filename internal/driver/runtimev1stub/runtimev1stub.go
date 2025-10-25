package runtimev1stub

import (
    "context"
    "errors"

    "google.golang.org/grpc"
)

// ExecuteRequest mirrors the fields used by the agent.
type ExecuteRequest struct {
    Instruction string
    SubjectId   string
    Params      map[string]string
    ExecutionId string
}

// ExecuteReply mirrors the fields used by the agent.
type ExecuteReply struct {
    ExitCode   int32
    StdoutTail string
    StderrTail string
    Artifacts  map[string]string
}

// RuntimeDriverClient is a minimal interface for the driver client.
type RuntimeDriverClient interface {
    Execute(ctx context.Context, in *ExecuteRequest, opts ...grpc.CallOption) (*ExecuteReply, error)
}

// NewRuntimeDriverClient returns a stub client that always errors. Replace this
// package with the real driver API bindings when available.
func NewRuntimeDriverClient(_ *grpc.ClientConn) RuntimeDriverClient {
    return &stubClient{}
}

type stubClient struct{}

func (s *stubClient) Execute(ctx context.Context, in *ExecuteRequest, opts ...grpc.CallOption) (*ExecuteReply, error) {
    return nil, errors.New("runtimev1 stub: please provide real driver module")
}

