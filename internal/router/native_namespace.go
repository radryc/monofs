package router

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"syscall"
	"time"

	pb "github.com/radryc/monofs/api/proto"
	"github.com/radryc/monofs/internal/monopath"
	"github.com/radryc/monofs/internal/sharding"
)

const (
	nativeNamespaceRPCTimeout    = 5 * time.Second
	nativeNamespaceEntryTTL      = 1 * time.Second
	nativeNamespaceAttrTTL       = 1 * time.Second
	nativeNamespaceDirTTL        = 1 * time.Second
	nativeNamespaceRouteTTL      = 30 * time.Second
	nativeNamespaceBlockSize     = 4096
	nativeNamespaceFallbackBytes = 4 * 1024 * 1024 * 1024 * 1024
	nativeNamespaceFreeFiles     = 1_000_000
)

// NativeTTLConfig describes default cache lifetimes that the native gateway can
// advertise to a kernel client during mount negotiation.
type NativeTTLConfig struct {
	EntryTTL time.Duration
	AttrTTL  time.Duration
	DirTTL   time.Duration
	RouteTTL time.Duration
}

// DefaultNativeTTLConfig returns the current default TTL policy for the native
// namespace/data path.
func DefaultNativeTTLConfig() NativeTTLConfig {
	return NativeTTLConfig{
		EntryTTL: nativeNamespaceEntryTTL,
		AttrTTL:  nativeNamespaceAttrTTL,
		DirTTL:   nativeNamespaceDirTTL,
		RouteTTL: nativeNamespaceRouteTTL,
	}
}

// NativeMountInfo is the initial namespace snapshot returned by the future
// native gateway mount handshake.
type NativeMountInfo struct {
	ClusterVersion      int64
	NamespaceGeneration int64
	GuardianVisible     bool
	Root                *pb.GetAttrResponse
	TTLs                NativeTTLConfig
}

// NativeStatFS contains filesystem statistics for native clients.
type NativeStatFS struct {
	Blocks              uint64
	Bfree               uint64
	Bavail              uint64
	Files               uint64
	Ffree               uint64
	Bsize               uint32
	Frsize              uint32
	NameLen             uint32
	ClusterVersion      int64
	NamespaceGeneration int64
}

type nativeNodeTarget struct {
	id     string
	node   sharding.Node
	client pb.MonoFSClient
}

// NativeMountInfo returns the initial root metadata and cache policy that the
// future native gateway will surface during mount negotiation.
func (r *Router) NativeMountInfo(ctx context.Context) (*NativeMountInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(r.nativeHealthyTargets()) == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	now := time.Now().Unix()
	version := r.version.Load()
	generation := int64(r.nativeEffectiveGeneration())

	return &NativeMountInfo{
		ClusterVersion:      version,
		NamespaceGeneration: generation,
		GuardianVisible:     r.isGuardianVisible(),
		Root: &pb.GetAttrResponse{
			Ino:   1,
			Mode:  0o755 | uint32(syscall.S_IFDIR),
			Size:  0,
			Mtime: now,
			Atime: now,
			Ctime: now,
			Nlink: 2,
			Uid:   uint32(1000),
			Gid:   uint32(1000),
			Found: true,
		},
		TTLs: DefaultNativeTTLConfig(),
	}, nil
}

// NativeLookup resolves a path using the same authoritative fallback semantics
// as the current sharded client, but from the router/gateway side.
func (r *Router) NativeLookup(ctx context.Context, path string) (*pb.LookupResponse, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	rankedTargets, _ := r.nativeRankedTargets(path, 3)
	if len(rankedTargets) == 0 {
		return nil, 0, fmt.Errorf("no healthy nodes available")
	}

	generation := int64(r.nativeEffectiveGeneration())
	var lastErr error
	for _, target := range rankedTargets {
		callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
		resp, err := target.client.Lookup(callCtx, &pb.LookupRequest{
			ParentPath: path,
			Name:       "",
		})
		cancel()

		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetFound() {
			return resp, generation, nil
		}
		break
	}

	for _, target := range r.nativeHealthyTargetsExcept(rankedTargets[0].id) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
		resp, err := target.client.Lookup(callCtx, &pb.LookupRequest{
			ParentPath: path,
			Name:       "",
		})
		cancel()
		if err != nil {
			continue
		}
		if resp.GetFound() {
			return resp, generation, nil
		}
	}

	if lastErr != nil {
		for _, target := range r.nativeRouteTargets(ctx, path) {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}

			callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
			resp, err := target.client.Lookup(callCtx, &pb.LookupRequest{
				ParentPath: path,
				Name:       "",
			})
			cancel()
			if err != nil {
				continue
			}
			if resp.GetFound() {
				return resp, generation, nil
			}
		}
		return nil, 0, lastErr
	}

	return &pb.LookupResponse{Found: false}, generation, nil
}

// NativeGetAttr fetches metadata for a path with authoritative fallback
// semantics suitable for a future native gateway.
func (r *Router) NativeGetAttr(ctx context.Context, path string) (*pb.GetAttrResponse, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	rankedTargets, _ := r.nativeRankedTargets(path, 3)
	if len(rankedTargets) == 0 {
		return nil, 0, fmt.Errorf("no healthy nodes available")
	}

	generation := int64(r.nativeEffectiveGeneration())
	var lastErr error
	for _, target := range rankedTargets {
		callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
		resp, err := target.client.GetAttr(callCtx, &pb.GetAttrRequest{
			Path: path,
		})
		cancel()

		if err != nil {
			lastErr = err
			continue
		}
		if resp.GetFound() {
			return resp, generation, nil
		}
		break
	}

	for _, target := range r.nativeHealthyTargetsExcept(rankedTargets[0].id) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
		resp, err := target.client.GetAttr(callCtx, &pb.GetAttrRequest{
			Path: path,
		})
		cancel()
		if err != nil {
			continue
		}
		if resp.GetFound() {
			return resp, generation, nil
		}
	}

	if lastErr != nil {
		for _, target := range r.nativeRouteTargets(ctx, path) {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}

			callCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
			resp, err := target.client.GetAttr(callCtx, &pb.GetAttrRequest{
				Path: path,
			})
			cancel()
			if err != nil {
				continue
			}
			if resp.GetFound() {
				return resp, generation, nil
			}
		}
		return nil, 0, lastErr
	}

	return &pb.GetAttrResponse{Found: false}, generation, nil
}

// NativeReadDir returns the authoritative merged directory listing across all
// healthy nodes. It never returns a partial listing as success.
func (r *Router) NativeReadDir(ctx context.Context, path string) ([]*pb.DirEntry, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	targets := r.nativeHealthyTargets()
	if len(targets) == 0 {
		return nil, 0, fmt.Errorf("no healthy nodes available")
	}

	generation := int64(r.nativeEffectiveGeneration())
	type nodeResult struct {
		nodeID  string
		entries []*pb.DirEntry
		err     error
	}

	results := make([]nodeResult, len(targets))
	var wg sync.WaitGroup

	for i, target := range targets {
		wg.Add(1)
		go func(idx int, nodeID string, client pb.MonoFSClient) {
			defer wg.Done()
			entries, err := r.nativeReadDirFromNode(ctx, client, nodeID, path)
			results[idx] = nodeResult{nodeID: nodeID, entries: entries, err: err}
		}(i, target.id, target.client)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	allEntries := make(map[string]*pb.DirEntry)
	var failedNodes []int

	for i, result := range results {
		if result.err != nil {
			failedNodes = append(failedNodes, i)
			continue
		}
		for _, entry := range result.entries {
			if _, exists := allEntries[entry.GetName()]; !exists {
				allEntries[entry.GetName()] = entry
			}
		}
	}

	if len(failedNodes) > 0 && ctx.Err() == nil {
		for _, idx := range failedNodes {
			entries, err := r.nativeReadDirFromNode(ctx, targets[idx].client, targets[idx].id, path)
			results[idx] = nodeResult{nodeID: targets[idx].id, entries: entries, err: err}
		}

		for _, idx := range failedNodes {
			result := results[idx]
			if result.err != nil {
				return nil, 0, fmt.Errorf("readdir incomplete: node %s failed: %w", result.nodeID, result.err)
			}
			for _, entry := range result.entries {
				if _, exists := allEntries[entry.GetName()]; !exists {
					allEntries[entry.GetName()] = entry
				}
			}
		}
	}

	entries := make([]*pb.DirEntry, 0, len(allEntries))
	for _, entry := range allEntries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].GetName() < entries[j].GetName()
	})

	return entries, generation, nil
}

// NativeStatFS returns filesystem statistics aggregated from the router's
// healthy active nodes.
func (r *Router) NativeStatFS(ctx context.Context) (*NativeStatFS, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var totalBytes uint64
	var freeBytes uint64
	var totalFiles uint64
	var healthyCount int

	for _, state := range r.nodes {
		if state == nil || state.info == nil || !state.info.Healthy || state.status != NodeActive {
			continue
		}
		healthyCount++

		total := uint64FromInt64(maxInt64(state.diskTotalBytes, state.info.GetDiskTotalBytes()))
		free := uint64FromInt64(maxInt64(state.diskFreeBytes, state.info.GetDiskFreeBytes()))
		files := uint64FromInt64(maxInt64(state.ownedFilesCount, state.info.GetTotalFiles()))

		totalBytes += total
		freeBytes += free
		totalFiles += files
	}

	if healthyCount == 0 {
		return nil, fmt.Errorf("no healthy nodes available")
	}

	if totalBytes == 0 {
		totalBytes = nativeNamespaceFallbackBytes
		freeBytes = nativeNamespaceFallbackBytes / 2
	}
	if freeBytes > totalBytes {
		freeBytes = totalBytes
	}

	files := totalFiles + nativeNamespaceFreeFiles
	version := r.version.Load()
	generation := int64(r.nativeEffectiveGeneration())

	return &NativeStatFS{
		Blocks:              ceilDiv(totalBytes, nativeNamespaceBlockSize),
		Bfree:               ceilDiv(freeBytes, nativeNamespaceBlockSize),
		Bavail:              ceilDiv(freeBytes, nativeNamespaceBlockSize),
		Files:               files,
		Ffree:               nativeNamespaceFreeFiles,
		Bsize:               nativeNamespaceBlockSize,
		Frsize:              nativeNamespaceBlockSize,
		NameLen:             255,
		ClusterVersion:      version,
		NamespaceGeneration: generation,
	}, nil
}

func (r *Router) nativeHealthyTargets() []nativeNodeTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	targets := make([]nativeNodeTarget, 0, len(r.nodes))
	for nodeID, state := range r.nodes {
		if state == nil || state.info == nil || state.client == nil {
			continue
		}
		if !state.info.Healthy || state.status != NodeActive {
			continue
		}

		weight := state.info.GetWeight()
		if weight == 0 {
			weight = 1
		}

		targets = append(targets, nativeNodeTarget{
			id: nodeID,
			node: sharding.Node{
				ID:      nodeID,
				Address: state.info.GetAddress(),
				Weight:  weight,
				Healthy: true,
			},
			client: state.client,
		})
	}

	sort.Slice(targets, func(i, j int) bool {
		return targets[i].id < targets[j].id
	})
	return targets
}

func (r *Router) nativeHealthyTargetsExcept(excludeNodeID string) []nativeNodeTarget {
	targets := r.nativeHealthyTargets()
	filtered := make([]nativeNodeTarget, 0, len(targets))
	for _, target := range targets {
		if target.id != excludeNodeID {
			filtered = append(filtered, target)
		}
	}
	return filtered
}

func (r *Router) nativeRankedTargets(path string, max int) ([]nativeNodeTarget, string) {
	targets := r.nativeHealthyTargets()
	if len(targets) == 0 {
		return nil, ""
	}

	nodes := make([]sharding.Node, 0, len(targets))
	byID := make(map[string]nativeNodeTarget, len(targets))
	for _, target := range targets {
		nodes = append(nodes, target.node)
		byID[target.id] = target
	}

	key := monopath.BuildShardKey(path)
	if key == "" {
		key = "/"
	}

	rankedNodes := sharding.NewHRW(nodes).GetNodes(key, max)
	rankedTargets := make([]nativeNodeTarget, 0, len(rankedNodes))
	for _, node := range rankedNodes {
		if target, ok := byID[node.ID]; ok {
			rankedTargets = append(rankedTargets, target)
		}
	}

	return rankedTargets, key
}

func (r *Router) nativeRouteTargets(ctx context.Context, path string) []nativeNodeTarget {
	displayPath, filePath, ok := monopath.SplitDisplayPath(path)
	if !ok || filePath == "" {
		return nil
	}

	resp, err := r.GetNodeForFile(ctx, &pb.GetNodeForFileRequest{
		StorageId: sharding.GenerateStorageID(displayPath),
		FilePath:  filePath,
	})
	if err != nil {
		return nil
	}

	allTargets := r.nativeHealthyTargets()
	byID := make(map[string]nativeNodeTarget, len(allTargets))
	for _, target := range allTargets {
		byID[target.id] = target
	}

	orderedIDs := append([]string{resp.GetNodeId()}, resp.GetFallbackNodeIds()...)
	ordered := make([]nativeNodeTarget, 0, len(orderedIDs))
	seen := make(map[string]struct{}, len(orderedIDs))
	for _, nodeID := range orderedIDs {
		if nodeID == "" {
			continue
		}
		if _, exists := seen[nodeID]; exists {
			continue
		}
		target, ok := byID[nodeID]
		if !ok {
			continue
		}
		seen[nodeID] = struct{}{}
		ordered = append(ordered, target)
	}

	return ordered
}

func (r *Router) nativeReadDirFromNode(ctx context.Context, client pb.MonoFSClient, nodeID, path string) ([]*pb.DirEntry, error) {
	nodeCtx, cancel := context.WithTimeout(ctx, nativeNamespaceRPCTimeout)
	defer cancel()

	stream, err := client.ReadDir(nodeCtx, &pb.ReadDirRequest{Path: path})
	if err != nil {
		return nil, fmt.Errorf("readdir RPC to node %s: %w", nodeID, err)
	}

	var entries []*pb.DirEntry
	for {
		entry, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return entries, fmt.Errorf("readdir stream from node %s: %w", nodeID, err)
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func ceilDiv(value uint64, divisor uint32) uint64 {
	if value == 0 {
		return 0
	}
	return (value + uint64(divisor) - 1) / uint64(divisor)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func uint64FromInt64(v int64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}
