package driver

import (
    "context"
    "time"

	runtimev1 "github.com/WangQiHao-Charlie/driver/api/proto/runtime/v1"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

type GRPCConfig struct {
	Addr        string
	Insecure    bool
	DialTimeout time.Duration
}

type GRPCDriver struct {
	cfg    GRPCConfig
	conn   *grpc.ClientConn
	client runtimev1.RuntimeDriverClient
}

func NewGRPCDriver(ctx context.Context, cfg GRPCConfig) (*GRPCDriver, error) {
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	var opts []grpc.DialOption
	if cfg.Insecure {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	conn, err := grpc.DialContext(dctx, cfg.Addr, append(opts, grpc.WithBlock())...)
	if err != nil {
		return nil, err
	}
	c := runtimev1.NewRuntimeDriverClient(conn)
	return &GRPCDriver{cfg: cfg, conn: conn, client: c}, nil
}

func (d *GRPCDriver) Close() error {
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}

type ExecuteResult struct {
	ExitCode   int32
	StdoutTail string
	StderrTail string
	Artifacts  map[string]string
}

func (d *GRPCDriver) Execute(ctx context.Context, instruction, subjectID, executionID string, params map[string]string) (*ExecuteResult, error) {
	req := &runtimev1.ExecuteRequest{
		Instruction: instruction,
		SubjectId:   subjectID,
		Params:      params,
		ExecutionId: executionID,
	}
	resp, err := d.client.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	return &ExecuteResult{
		ExitCode:   resp.ExitCode,
		StdoutTail: resp.StdoutTail,
		StderrTail: resp.StderrTail,
		Artifacts:  resp.Artifacts,
	}, nil
}
