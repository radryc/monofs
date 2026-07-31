package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/radryc/monofs/internal/telemetry"
	"github.com/radryc/monofs/internal/worker"
	"github.com/radryc/monofs/internal/worker/executor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	routerAddr    = flag.String("router", "localhost:8081", "router gRPC address")
	workerType    = flag.String("type", "builder", "worker type: builder, docker, deployer")
	concurrency   = flag.Int("concurrency", 4, "max concurrent task executions")
	guardianToken = flag.String("guardian-token", "", "guardian token for KVS operations")
	workerID      = flag.String("id", "", "worker unique ID (auto-generated if empty)")
	logLevel      = flag.String("log-level", "info", "log level: debug, info, warn, error")
	mountPath     = flag.String("mount-path", "/mnt/monofs", "monofs FUSE mount path for source access")
	executorPort  = flag.Int("executor-port", 0, "port for REAPI executor HTTP server (0 = disabled)")
	cacheAddr     = flag.String("cache-addr", "", "monofs-cache address for executor (required if --executor-port set)")
)

func main() {
	flag.Parse()

	if *guardianToken == "" {
		*guardianToken = os.Getenv("MONOFS_GUARDIAN_TOKEN")
	}

	var level slog.Level
	switch strings.ToLower(*logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	logger := slog.New(h).With("worker_type", *workerType)

	comp := "pipeline-worker-" + *workerType
	telemetryCfg, _ := telemetry.LoadConfig(comp)
	telemetryHandle, err := telemetry.Setup(context.Background(), telemetryCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup telemetry: %v\n", err)
	}
	if telemetryHandle.Enabled() {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := telemetryHandle.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(os.Stderr, "shutdown telemetry: %v\n", err)
			}
		}()
		handler := telemetry.WrapSlogHandler(h, "worker/"+comp)
		logger = slog.New(handler).With("worker_type", *workerType)
	}

	logger.Info("starting pipeline worker",
		"router", *routerAddr,
		"concurrency", *concurrency,
	)

	conn, err := grpc.NewClient(*routerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64*1024*1024)),
		grpc.WithConnectParams(grpc.ConnectParams{
			MinConnectTimeout: 5 * time.Second,
		}),
	)
	if err != nil {
		logger.Error("connect to router", "error", err)
		os.Exit(1)
	}
	defer conn.Close()

	client, err := worker.NewClient(conn, *guardianToken, *workerID, logger)
	if err != nil {
		logger.Error("create worker client", "error", err)
		os.Exit(1)
	}

	var handler worker.Handler
	switch *workerType {
	case "builder":
		handler = worker.NewBuilderHandler(*mountPath, logger)
	case "docker":
		handler = worker.NewDockerHandler(logger)
	case "deployer":
		handler = worker.NewDeployerHandler(*mountPath, logger)
	case "bazel":
		handler = worker.NewBazelHandler(*mountPath, *cacheAddr, "", logger)
	default:
		logger.Error("unknown worker type", "type", *workerType)
		os.Exit(1)
	}

	w := worker.New(client, handler, *concurrency, logger)

	// Start executor HTTP server if --executor-port is set.
	if *executorPort > 0 {
		if *cacheAddr == "" {
			logger.Error("--cache-addr is required when --executor-port is set")
			os.Exit(1)
		}
		exec := executor.New(executor.Config{
			ID:        *workerID,
			CacheAddr: *cacheAddr,
			WorkDir:   filepath.Join(os.TempDir(), "monofs-executor"),
			MaxJobs:   *concurrency,
			Platform: executor.Platform{
				OS:   "linux",
				Arch: "amd64",
				Pool: *workerType,
			},
			Logger: logger,
		})
		go startExecutorServer(exec, *executorPort, logger)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		logger.Info("shutting down")
		cancel()
	}()

	if err := w.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("worker stopped with error", "error", err)
		os.Exit(1)
	}

	logger.Info("worker stopped")
}

// startExecutorServer runs an HTTP server that accepts ExecuteRequests
// and dispatches them to the executor. The API is:
//
//	POST /execute  {"arguments":[...], "env":[...], "input_files":[...], ...}
//	GET  /status   {"worker_id":"...", "total_execs":..., ...}
func startExecutorServer(exec *executor.Executor, port int, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/execute", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var req struct {
			Arguments         []string             `json:"arguments"`
			Env               []string             `json:"environment_variables"`
			InputFiles        []executor.InputFile `json:"input_files"`
			OutputFiles       []string             `json:"output_files"`
			OutputDirectories []string             `json:"output_directories"`
			WorkingDirectory  string               `json:"working_directory"`
			TimeoutSec        int                  `json:"timeout_sec"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}

		timeout := time.Duration(req.TimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		wd := req.WorkingDirectory
		if wd == "" {
			wd = fmt.Sprintf("job-%d", time.Now().UnixNano())
		}

		execReq := &executor.ExecuteRequest{
			Arguments:            req.Arguments,
			EnvironmentVariables: req.Env,
			InputFiles:           req.InputFiles,
			OutputFiles:          req.OutputFiles,
			OutputDirectories:    req.OutputDirectories,
			WorkingDirectory:     wd,
			Timeout:              timeout,
		}

		result, err := exec.Execute(r.Context(), execReq)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": false, "error": err.Error(),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":                 result.ExitCode == 0 && result.Error == "",
			"exit_code":          result.ExitCode,
			"stdout":             string(result.Stdout),
			"stderr":             string(result.Stderr),
			"output_files":       result.OutputFiles,
			"output_directories": result.OutputDirectories,
			"duration_ms":        result.Duration.Milliseconds(),
			"error":              result.Error,
		})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := exec.Status()
		json.NewEncoder(w).Encode(status)
	})

	addr := fmt.Sprintf(":%d", port)
	logger.Info("executor HTTP server listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("executor server error", "error", err)
	}
}
