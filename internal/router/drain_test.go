package router

import (
	"context"
	"log/slog"
	"testing"

	pb "github.com/radryc/monofs/api/proto"
)

func TestDrainAndUndrainCluster(t *testing.T) {
	r := &Router{logger: slog.New(slog.DiscardHandler)}
	ctx := context.Background()

	if r.IsDrained() {
		t.Fatal("new router is already drained")
	}

	resp, err := r.DrainCluster(ctx, &pb.DrainClusterRequest{})
	if err != nil {
		t.Fatalf("DrainCluster: %v", err)
	}
	if !resp.GetSuccess() || !r.IsDrained() {
		t.Fatalf("drain response = %+v, drained=%v", resp, r.IsDrained())
	}
	if resp.GetDrainedAt() == 0 {
		t.Fatal("drained_at not set")
	}
	if r.drainReason != "planned maintenance" {
		t.Fatalf("default drain reason = %q", r.drainReason)
	}

	// Draining twice is reported, not applied again.
	resp, err = r.DrainCluster(ctx, &pb.DrainClusterRequest{Reason: "upgrade"})
	if err != nil {
		t.Fatalf("second DrainCluster: %v", err)
	}
	if resp.GetSuccess() {
		t.Fatalf("second drain succeeded: %+v", resp)
	}

	undrain, err := r.UndrainCluster(ctx, &pb.UndrainClusterRequest{})
	if err != nil {
		t.Fatalf("UndrainCluster: %v", err)
	}
	if !undrain.GetSuccess() || r.IsDrained() {
		t.Fatalf("undrain response = %+v, drained=%v", undrain, r.IsDrained())
	}
	if r.drainReason != "" {
		t.Fatalf("drain reason not cleared: %q", r.drainReason)
	}

	undrain, err = r.UndrainCluster(ctx, &pb.UndrainClusterRequest{})
	if err != nil {
		t.Fatalf("second UndrainCluster: %v", err)
	}
	if undrain.GetSuccess() {
		t.Fatalf("second undrain succeeded: %+v", undrain)
	}
}

func TestDrainClusterUsesProvidedReason(t *testing.T) {
	r := &Router{logger: slog.New(slog.DiscardHandler)}
	resp, err := r.DrainCluster(context.Background(), &pb.DrainClusterRequest{Reason: "node replacement"})
	if err != nil {
		t.Fatalf("DrainCluster: %v", err)
	}
	if r.drainReason != "node replacement" {
		t.Fatalf("reason = %q", r.drainReason)
	}
	if resp.GetMessage() == "" {
		t.Fatal("drain message is empty")
	}
}
