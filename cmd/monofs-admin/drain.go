package main

import (
	"context"
	"fmt"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// drainCluster puts the cluster into drain mode.
func drainCluster(routerAddr, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	resp, err := client.DrainCluster(ctx, &pb.DrainClusterRequest{
		Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("drain cluster: %w", err)
	}

	if !resp.Success {
		fmt.Printf("⚠️  %s\n", resp.Message)
		return nil
	}

	fmt.Printf("\n")
	fmt.Printf("╔══════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                      CLUSTER DRAIN MODE                          ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")
	fmt.Printf("🚧 Cluster is now in drain mode\n")
	fmt.Printf("📝 Reason: %s\n", reason)
	fmt.Printf("🕐 Started: %s\n", time.Unix(resp.DrainedAt, 0).Format(time.RFC3339))
	fmt.Printf("\n")
	fmt.Printf("⚠️  Failover is DISABLED\n")
	fmt.Printf("⚠️  Nodes can be safely shut down without triggering failover\n")
	fmt.Printf("\n")
	fmt.Printf("Safe shutdown commands:\n")
	fmt.Printf("  docker-compose down\n")
	fmt.Printf("  make docker-down\n")
	fmt.Printf("\n")
	fmt.Printf("To exit drain mode: monofs-admin undrain --router %s\n", routerAddr)
	fmt.Printf("\n")

	return nil
}

// undrainCluster exits drain mode.
func undrainCluster(routerAddr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect to router: %w", err)
	}
	defer conn.Close()

	client := pb.NewMonoFSRouterClient(conn)

	resp, err := client.UndrainCluster(ctx, &pb.UndrainClusterRequest{})
	if err != nil {
		return fmt.Errorf("undrain cluster: %w", err)
	}

	if !resp.Success {
		fmt.Printf("⚠️  %s\n", resp.Message)
		return nil
	}

	fmt.Printf("\n")
	fmt.Printf("╔══════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                   CLUSTER NORMAL OPERATION                       ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")
	fmt.Printf("✅ Cluster exited drain mode\n")
	fmt.Printf("✅ Failover is ENABLED\n")
	fmt.Printf("✅ Normal operations resumed\n")
	fmt.Printf("📊 %s\n", resp.Message)
	fmt.Printf("\n")

	return nil
}
