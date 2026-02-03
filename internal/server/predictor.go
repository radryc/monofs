package server

import (
	"container/ring"
	"context"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/radryc/monofs/internal/fetcher"
)

// Predictor implements predictive prefetching using Markov chains and clustering.
// It runs on storage nodes and tracks access patterns to predict future accesses.
type Predictor struct {
	mu sync.RWMutex

	// Per-storageID Markov chains
	markovChains map[string]*markovChain

	// Directory-based predictions
	dirAccess map[string]*directoryAccess // storageID:dir -> access info

	// Global access ring buffer (for temporal patterns)
	recentAccesses *ring.Ring
	recentMu       sync.Mutex

	// Fetcher client for prefetch requests
	fetcherClient *fetcher.Client

	// Configuration
	config PredictorConfig

	logger *slog.Logger

	// Stats
	predictions  int64
	prefetches   int64
	prefetchHits int64
}

// PredictorConfig configures the prediction engine.
type PredictorConfig struct {
	// Markov chain settings
	MaxTransitionsPerFile int     // Max edges per source file
	TransitionDecayRate   float64 // Decay per hour (e.g., 0.95)
	MinTransitionCount    int     // Min transitions before predicting
	MarkovDepth           int     // How many hops to look ahead

	// Directory prediction
	DirectoryPrefetchSize int     // Max files to prefetch per directory
	DirectoryThreshold    float64 // Min access ratio to prefetch

	// Temporal settings
	SessionTimeout time.Duration // Gap that ends a session
	RecentWindow   int           // Number of recent accesses to track

	// Prefetch settings
	PrefetchThreshold float64 // Min probability to prefetch
	MaxPrefetchFiles  int     // Max files per prediction
	PrefetchPriority  int     // Priority for prefetch requests (0-10)

	// Cleanup
	CleanupInterval time.Duration
	MaxChainAge     time.Duration

	// Filtering
	IgnoreClientIDs []string // Client IDs to ignore (e.g., "search-indexer")
}

// DefaultPredictorConfig returns sensible defaults.
func DefaultPredictorConfig() PredictorConfig {
	return PredictorConfig{
		MaxTransitionsPerFile: 100,
		TransitionDecayRate:   0.95,
		MinTransitionCount:    3,
		MarkovDepth:           2,
		DirectoryPrefetchSize: 10,
		DirectoryThreshold:    0.3,
		SessionTimeout:        30 * time.Second,
		RecentWindow:          10000,
		PrefetchThreshold:     0.3,
		MaxPrefetchFiles:      10,
		PrefetchPriority:      5,
		CleanupInterval:       5 * time.Minute,
		MaxChainAge:           24 * time.Hour,
	}
}

type markovChain struct {
	storageID string

	// Transition matrix: fromFile -> toFile -> edge
	transitions map[string]map[string]*transitionEdge

	// Per-client session tracking
	sessions map[string]*clientSession

	// Stats
	totalTransitions int64
	lastUpdate       time.Time

	mu sync.RWMutex
}

type transitionEdge struct {
	count       int64
	lastSeen    time.Time
	decayFactor float64
}

type clientSession struct {
	lastFile   string
	lastAccess time.Time
	files      []string // Files accessed in this session
}

type directoryAccess struct {
	storageID string
	dirPath   string

	// File access counts within directory
	fileCounts map[string]int64

	// Total accesses
	totalAccesses int64

	lastUpdate time.Time
}

// NewPredictor creates a new prediction engine.
func NewPredictor(fetcherClient *fetcher.Client, config PredictorConfig, logger *slog.Logger) *Predictor {
	p := &Predictor{
		markovChains:   make(map[string]*markovChain),
		dirAccess:      make(map[string]*directoryAccess),
		recentAccesses: ring.New(config.RecentWindow),
		fetcherClient:  fetcherClient,
		config:         config,
		logger:         logger,
	}

	// Start cleanup goroutine
	go p.cleanupLoop()

	return p
}

// RecordAccess records a file access and triggers prediction.
func (p *Predictor) RecordAccess(ctx context.Context, storageID, filePath, clientID string, meta *BlobMeta) {
	// Check if this client should be ignored
	for _, ignoreID := range p.config.IgnoreClientIDs {
		if clientID == ignoreID {
			return
		}
	}

	now := time.Now()

	// Record in Markov chain
	p.recordMarkovTransition(storageID, filePath, clientID, now)

	// Record directory access
	p.recordDirectoryAccess(storageID, filePath, now)

	// Record in recent accesses
	p.recordRecentAccess(storageID, filePath, now)

	// Get predictions and trigger prefetch
	predictions := p.Predict(storageID, filePath)
	if len(predictions) > 0 && p.fetcherClient != nil {
		go p.triggerPrefetch(ctx, predictions, meta)
	}
}

func (p *Predictor) recordMarkovTransition(storageID, filePath, clientID string, now time.Time) {
	p.mu.Lock()
	chain, ok := p.markovChains[storageID]
	if !ok {
		chain = &markovChain{
			storageID:   storageID,
			transitions: make(map[string]map[string]*transitionEdge),
			sessions:    make(map[string]*clientSession),
		}
		p.markovChains[storageID] = chain
	}
	p.mu.Unlock()

	chain.mu.Lock()
	defer chain.mu.Unlock()

	// Get or create session
	session, ok := chain.sessions[clientID]
	if !ok || now.Sub(session.lastAccess) > p.config.SessionTimeout {
		// New session
		session = &clientSession{
			files: make([]string, 0, 100),
		}
		chain.sessions[clientID] = session
	}

	prevFile := session.lastFile
	session.lastFile = filePath
	session.lastAccess = now
	session.files = append(session.files, filePath)

	// Limit session size
	if len(session.files) > 100 {
		session.files = session.files[len(session.files)-100:]
	}

	// Record transition
	if prevFile != "" && prevFile != filePath {
		if chain.transitions[prevFile] == nil {
			chain.transitions[prevFile] = make(map[string]*transitionEdge)
		}

		edge, ok := chain.transitions[prevFile][filePath]
		if !ok {
			edge = &transitionEdge{decayFactor: 1.0}
			chain.transitions[prevFile][filePath] = edge
		}

		// Apply temporal decay
		if !edge.lastSeen.IsZero() {
			hours := now.Sub(edge.lastSeen).Hours()
			if hours > 0 {
				edge.decayFactor *= math.Pow(p.config.TransitionDecayRate, hours)
			}
		}

		edge.count++
		edge.lastSeen = now
		chain.totalTransitions++
		chain.lastUpdate = now

		// Prune if too many transitions
		if len(chain.transitions[prevFile]) > p.config.MaxTransitionsPerFile {
			p.pruneTransitions(chain, prevFile)
		}
	}
}

func (p *Predictor) pruneTransitions(chain *markovChain, fromFile string) {
	edges := make([]*transitionEdge, 0, len(chain.transitions[fromFile]))
	files := make([]string, 0, len(chain.transitions[fromFile]))

	for toFile, edge := range chain.transitions[fromFile] {
		edges = append(edges, edge)
		files = append(files, toFile)
	}

	// Sort by effective weight
	type weightedEdge struct {
		file   string
		weight float64
	}
	weighted := make([]weightedEdge, len(edges))
	for i := range edges {
		weighted[i] = weightedEdge{
			file:   files[i],
			weight: float64(edges[i].count) * edges[i].decayFactor,
		}
	}
	sort.Slice(weighted, func(i, j int) bool {
		return weighted[i].weight > weighted[j].weight
	})

	// Keep top 75%
	keep := p.config.MaxTransitionsPerFile * 3 / 4
	newMap := make(map[string]*transitionEdge, keep)
	for i := 0; i < keep && i < len(weighted); i++ {
		newMap[weighted[i].file] = chain.transitions[fromFile][weighted[i].file]
	}
	chain.transitions[fromFile] = newMap
}

func (p *Predictor) recordDirectoryAccess(storageID, filePath string, now time.Time) {
	dirPath := getDirectory(filePath)
	key := storageID + ":" + dirPath

	p.mu.Lock()
	da, ok := p.dirAccess[key]
	if !ok {
		da = &directoryAccess{
			storageID:  storageID,
			dirPath:    dirPath,
			fileCounts: make(map[string]int64),
		}
		p.dirAccess[key] = da
	}
	p.mu.Unlock()

	da.fileCounts[filePath]++
	da.totalAccesses++
	da.lastUpdate = now
}

type recentAccess struct {
	storageID string
	filePath  string
	timestamp time.Time
}

func (p *Predictor) recordRecentAccess(storageID, filePath string, now time.Time) {
	p.recentMu.Lock()
	defer p.recentMu.Unlock()

	p.recentAccesses.Value = &recentAccess{
		storageID: storageID,
		filePath:  filePath,
		timestamp: now,
	}
	p.recentAccesses = p.recentAccesses.Next()
}

// Predict returns predicted files based on access patterns.
func (p *Predictor) Predict(storageID, filePath string) []PredictedFile {
	var predictions []PredictedFile

	// 1. Markov chain predictions
	markovPreds := p.predictMarkov(storageID, filePath)
	predictions = append(predictions, markovPreds...)

	// 2. Directory-based predictions
	dirPreds := p.predictDirectory(storageID, filePath)
	predictions = append(predictions, dirPreds...)

	// 3. Structural predictions (common files)
	structPreds := p.predictStructural(storageID, filePath)
	predictions = append(predictions, structPreds...)

	// Merge and deduplicate
	merged := p.mergePredictions(predictions)

	// Filter by threshold and limit
	result := make([]PredictedFile, 0, p.config.MaxPrefetchFiles)
	for _, pred := range merged {
		if pred.Probability >= p.config.PrefetchThreshold {
			result = append(result, pred)
			if len(result) >= p.config.MaxPrefetchFiles {
				break
			}
		}
	}

	p.predictions++
	return result
}

func (p *Predictor) predictMarkov(storageID, filePath string) []PredictedFile {
	p.mu.RLock()
	chain, ok := p.markovChains[storageID]
	p.mu.RUnlock()

	if !ok {
		return nil
	}

	chain.mu.RLock()
	defer chain.mu.RUnlock()

	edges := chain.transitions[filePath]
	if len(edges) == 0 {
		return nil
	}

	// Calculate total weight
	var totalWeight float64
	for _, edge := range edges {
		totalWeight += float64(edge.count) * edge.decayFactor
	}

	if totalWeight == 0 {
		return nil
	}

	// Build predictions
	predictions := make([]PredictedFile, 0, len(edges))
	for toFile, edge := range edges {
		if edge.count < int64(p.config.MinTransitionCount) {
			continue
		}
		prob := (float64(edge.count) * edge.decayFactor) / totalWeight
		if prob >= p.config.PrefetchThreshold/2 { // Include marginal predictions
			predictions = append(predictions, PredictedFile{
				StorageID:   storageID,
				FilePath:    toFile,
				Probability: prob,
				Source:      "markov",
			})
		}
	}

	// Recurse for depth > 1
	if p.config.MarkovDepth > 1 {
		secondLevel := make([]PredictedFile, 0)
		for _, pred := range predictions {
			if pred.Probability >= p.config.PrefetchThreshold {
				subEdges := chain.transitions[pred.FilePath]
				for toFile, edge := range subEdges {
					subProb := pred.Probability * (float64(edge.count) * edge.decayFactor / totalWeight) * 0.7
					if subProb >= p.config.PrefetchThreshold/2 {
						secondLevel = append(secondLevel, PredictedFile{
							StorageID:   storageID,
							FilePath:    toFile,
							Probability: subProb,
							Source:      "markov_l2",
						})
					}
				}
			}
		}
		predictions = append(predictions, secondLevel...)
	}

	return predictions
}

func (p *Predictor) predictDirectory(storageID, filePath string) []PredictedFile {
	dirPath := getDirectory(filePath)
	key := storageID + ":" + dirPath

	p.mu.RLock()
	da, ok := p.dirAccess[key]
	p.mu.RUnlock()

	if !ok || da.totalAccesses < 5 {
		return nil
	}

	predictions := make([]PredictedFile, 0, p.config.DirectoryPrefetchSize)

	for file, count := range da.fileCounts {
		if file == filePath {
			continue
		}
		ratio := float64(count) / float64(da.totalAccesses)
		if ratio >= p.config.DirectoryThreshold {
			predictions = append(predictions, PredictedFile{
				StorageID:   storageID,
				FilePath:    file,
				Probability: ratio * 0.6, // Directory prediction is weaker signal
				Source:      "directory",
			})
		}
	}

	// Sort and limit
	sort.Slice(predictions, func(i, j int) bool {
		return predictions[i].Probability > predictions[j].Probability
	})
	if len(predictions) > p.config.DirectoryPrefetchSize {
		predictions = predictions[:p.config.DirectoryPrefetchSize]
	}

	return predictions
}

func (p *Predictor) predictStructural(storageID, filePath string) []PredictedFile {
	var predictions []PredictedFile

	// Common patterns based on file extension
	ext := getExtension(filePath)
	dir := getDirectory(filePath)

	// Go files often access go.mod, go.sum
	if ext == ".go" {
		predictions = append(predictions,
			PredictedFile{StorageID: storageID, FilePath: "go.mod", Probability: 0.4, Source: "structural"},
			PredictedFile{StorageID: storageID, FilePath: "go.sum", Probability: 0.3, Source: "structural"},
		)
	}

	// README is commonly accessed
	predictions = append(predictions,
		PredictedFile{StorageID: storageID, FilePath: dir + "/README.md", Probability: 0.25, Source: "structural"},
		PredictedFile{StorageID: storageID, FilePath: "README.md", Probability: 0.2, Source: "structural"},
	)

	return predictions
}

func (p *Predictor) mergePredictions(predictions []PredictedFile) []PredictedFile {
	merged := make(map[string]*PredictedFile)

	for _, pred := range predictions {
		key := pred.StorageID + ":" + pred.FilePath
		existing, ok := merged[key]
		if !ok || pred.Probability > existing.Probability {
			p := pred // Copy
			merged[key] = &p
		}
	}

	result := make([]PredictedFile, 0, len(merged))
	for _, pred := range merged {
		result = append(result, *pred)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Probability > result[j].Probability
	})

	return result
}

// BlobMeta contains metadata for prefetch requests.
type BlobMeta struct {
	BlobHash   string
	RepoURL    string
	Branch     string
	SourceType fetcher.SourceType
	ModulePath string // For Go modules
	Version    string // For Go modules
}

func (p *Predictor) triggerPrefetch(ctx context.Context, predictions []PredictedFile, currentMeta *BlobMeta) {
	if currentMeta == nil || p.fetcherClient == nil {
		return
	}

	requests := make([]*fetcher.FetchRequest, 0, len(predictions))

	for _, pred := range predictions {
		req := &fetcher.FetchRequest{
			ContentID: pred.ContentID,
			SourceKey: pred.StorageID,
			Priority:  p.config.PrefetchPriority,
			SourceConfig: map[string]string{
				"repo_url": currentMeta.RepoURL,
				"branch":   currentMeta.Branch,
			},
		}

		if currentMeta.SourceType == fetcher.SourceTypeGoMod {
			req.SourceConfig["module_path"] = currentMeta.ModulePath
			req.SourceConfig["version"] = currentMeta.Version
		}

		requests = append(requests, req)
	}

	if err := p.fetcherClient.Prefetch(ctx, requests, currentMeta.SourceType); err != nil {
		p.logger.Warn("prefetch request failed", "error", err)
	} else {
		p.prefetches += int64(len(requests))
	}
}

func (p *Predictor) cleanupLoop() {
	ticker := time.NewTicker(p.config.CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		p.cleanup()
	}
}

func (p *Predictor) cleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// Cleanup old Markov chains
	for storageID, chain := range p.markovChains {
		chain.mu.Lock()
		if now.Sub(chain.lastUpdate) > p.config.MaxChainAge {
			delete(p.markovChains, storageID)
		} else {
			// Cleanup old sessions
			for clientID, session := range chain.sessions {
				if now.Sub(session.lastAccess) > p.config.SessionTimeout*10 {
					delete(chain.sessions, clientID)
				}
			}
		}
		chain.mu.Unlock()
	}

	// Cleanup old directory accesses
	for key, da := range p.dirAccess {
		if now.Sub(da.lastUpdate) > p.config.MaxChainAge {
			delete(p.dirAccess, key)
		}
	}
}

// GetStats returns predictor statistics.
func (p *Predictor) GetStats() PredictorStats {
	p.mu.RLock()
	chains := len(p.markovChains)
	dirs := len(p.dirAccess)
	p.mu.RUnlock()

	return PredictorStats{
		MarkovChains:  chains,
		DirectoryMaps: dirs,
		Predictions:   p.predictions,
		Prefetches:    p.prefetches,
		PrefetchHits:  p.prefetchHits,
	}
}

// PredictedFile represents a prefetch candidate.
type PredictedFile struct {
	StorageID   string
	FilePath    string
	Probability float64
	Source      string // "markov", "directory", "structural"
	ContentID   string // Blob hash (filled by caller if known)
}

// PredictorStats holds prediction statistics.
type PredictorStats struct {
	MarkovChains  int
	DirectoryMaps int
	Predictions   int64
	Prefetches    int64
	PrefetchHits  int64
}

// Helper functions

func getDirectory(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}

func getExtension(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i:]
		}
		if path[i] == '/' {
			break
		}
	}
	return ""
}
