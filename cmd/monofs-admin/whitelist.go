package main

import (
	"context"
	"fmt"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// manageWhitelist performs whitelist operations via gRPC.
func manageWhitelist(routerAddr, action, clientID, label string) error {
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

	switch action {
	case "list":
		return whitelistList(ctx, client)
	case "add":
		if clientID == "" {
			return fmt.Errorf("--client-id is required for add action")
		}
		return whitelistAdd(ctx, client, clientID, label)
	case "remove":
		if clientID == "" {
			return fmt.Errorf("--client-id is required for remove action")
		}
		return whitelistRemove(ctx, client, clientID)
	case "enable":
		return whitelistSetEnabled(ctx, client, true)
	case "disable":
		return whitelistSetEnabled(ctx, client, false)
	default:
		return fmt.Errorf("unknown whitelist action: %s (expected: add, remove, list, enable, disable)", action)
	}
}

func whitelistList(ctx context.Context, client pb.MonoFSRouterClient) error {
	resp, err := client.GetWhitelistStatus(ctx, &pb.GetWhitelistStatusRequest{})
	if err != nil {
		return fmt.Errorf("get whitelist status: %w", err)
	}

	fmt.Printf("\n")
	fmt.Printf("╔══════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║                     INGESTION WHITELIST                          ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════════════════════╝\n")
	fmt.Printf("\n")

	if resp.Enabled {
		fmt.Printf("🔒 Whitelist: ENABLED (only whitelisted clients can ingest)\n")
	} else {
		fmt.Printf("🔓 Whitelist: DISABLED (all clients can ingest)\n")
	}
	fmt.Printf("📋 Whitelisted clients: %d\n\n", resp.WhitelistedCount)

	if len(resp.Clients) == 0 {
		fmt.Printf("   (no clients whitelisted)\n\n")
		return nil
	}

	fmt.Printf("%-38s %-20s %s\n", "CLIENT ID", "LABEL", "ADDED AT")
	fmt.Printf("%-38s %-20s %s\n", "--------------------------------------", "--------------------", "-------------------")
	for _, c := range resp.Clients {
		addedAt := time.Unix(c.AddedAt, 0).Format(time.RFC3339)
		label := c.Label
		if label == "" {
			label = "-"
		}
		fmt.Printf("%-38s %-20s %s\n", c.ClientId, label, addedAt)
	}
	fmt.Println()

	return nil
}

func whitelistAdd(ctx context.Context, client pb.MonoFSRouterClient, clientID, label string) error {
	resp, err := client.AddWhitelistedClient(ctx, &pb.AddWhitelistedClientRequest{
		ClientId: clientID,
		Label:    label,
	})
	if err != nil {
		return fmt.Errorf("add whitelisted client: %w", err)
	}

	if !resp.Success {
		fmt.Printf("⚠️  %s\n", resp.Message)
		return nil
	}

	fmt.Printf("✅ %s\n", resp.Message)
	return nil
}

func whitelistRemove(ctx context.Context, client pb.MonoFSRouterClient, clientID string) error {
	resp, err := client.RemoveWhitelistedClient(ctx, &pb.RemoveWhitelistedClientRequest{
		ClientId: clientID,
	})
	if err != nil {
		return fmt.Errorf("remove whitelisted client: %w", err)
	}

	if !resp.Success {
		fmt.Printf("⚠️  %s\n", resp.Message)
		return nil
	}

	fmt.Printf("✅ %s\n", resp.Message)
	return nil
}

func whitelistSetEnabled(ctx context.Context, client pb.MonoFSRouterClient, enabled bool) error {
	resp, err := client.SetWhitelistEnabled(ctx, &pb.SetWhitelistEnabledRequest{
		Enabled: enabled,
	})
	if err != nil {
		return fmt.Errorf("set whitelist enabled: %w", err)
	}

	if !resp.Success {
		fmt.Printf("⚠️  %s\n", resp.Message)
		return nil
	}

	if enabled {
		fmt.Printf("🔒 %s\n", resp.Message)
	} else {
		fmt.Printf("🔓 %s\n", resp.Message)
	}
	return nil
}
