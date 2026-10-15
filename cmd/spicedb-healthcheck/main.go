// Command spicedb-healthcheck is a minimal gRPC health probe for SpiceDB.
//
// The authzed/spicedb OCI image contains only the spicedb binary (no shell,
// curl or wget), and SpiceDB v1.39.1 has no `healthcheck` subcommand, so the
// Docker Compose healthcheck cannot use CMD-SHELL. This binary is copied into
// the shadow-spicedb image and probes grpc.health.v1.Health/Check directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// probe dials addr, presents token as a Bearer credential, and returns the
// gRPC health status string. Any non-SERVING status is an error.
func probe(ctx context.Context, addr, token, service string) (string, error) {
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}

	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	resp, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: service})
	if err != nil {
		return "", fmt.Errorf("check %s: %w", addr, err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		return resp.GetStatus().String(), fmt.Errorf("check %s: status %s", addr, resp.GetStatus().String())
	}
	return resp.GetStatus().String(), nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:50051", "SpiceDB gRPC address")
	token := flag.String("token", "", "SpiceDB preshared key (sent as Bearer token)")
	service := flag.String("service", "", "gRPC health service name (empty = overall server health)")
	timeout := flag.Duration("timeout", 5*time.Second, "probe timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	status, err := probe(ctx, *addr, *token, *service)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spicedb-healthcheck: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("spicedb-healthcheck: %s %s\n", *addr, status)
}
