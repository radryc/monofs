package router

import pb "github.com/radryc/monofs/api/proto"

func normalizedKVSNodeStatus(status *pb.KVSNodeStatus) *pb.KVSNodeStatus {
	if status == nil {
		return &pb.KVSNodeStatus{Mode: "disabled", Role: "disabled"}
	}
	mode := status.GetMode()
	if mode == "" {
		mode = "disabled"
	}
	role := status.GetRole()
	if role == "" {
		role = "disabled"
	}
	return &pb.KVSNodeStatus{
		Enabled:   status.GetEnabled(),
		Healthy:   status.GetHealthy(),
		Mode:      mode,
		Role:      role,
		LeaderId:  status.GetLeaderId(),
		PeerCount: status.GetPeerCount(),
	}
}
