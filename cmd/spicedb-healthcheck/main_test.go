package main

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeHealthServer struct {
	grpc_health_v1.UnimplementedHealthServer
}

func (fakeHealthServer) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

// startHealthServer starts a real TCP gRPC server whose interceptor requires
// the Bearer token, and returns its address plus a stop function.
func startHealthServer(t *testing.T, wantToken string) (string, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	interceptor := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		got := ""
		if vals := md.Get("authorization"); len(vals) > 0 {
			got = vals[0]
		}
		if got != "Bearer "+wantToken {
			return nil, status.Error(codes.Unauthenticated, "bad token")
		}
		return handler(ctx, req)
	}

	srv := grpc.NewServer(grpc.UnaryInterceptor(interceptor))
	grpc_health_v1.RegisterHealthServer(srv, fakeHealthServer{})

	go func() {
		_ = srv.Serve(lis)
	}()

	return lis.Addr().String(), func() {
		srv.Stop()
		_ = lis.Close()
	}
}

func TestProbeServingWithToken(t *testing.T) {
	addr, stop := startHealthServer(t, "b2b-dev-key")
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	statusStr, err := probe(ctx, addr, "b2b-dev-key", "")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if statusStr != "SERVING" {
		t.Fatalf("status = %q, want SERVING", statusStr)
	}
}

func TestProbeRejectsWrongToken(t *testing.T) {
	addr, stop := startHealthServer(t, "b2b-dev-key")
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := probe(ctx, addr, "wrong-key", ""); err == nil {
		t.Fatal("probe succeeded with wrong token, want error")
	}
}
