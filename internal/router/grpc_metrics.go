package router

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

var (
	routerGRPCDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "grpc_duration_seconds",
		Help:      "Latency of router gRPC handlers, by method and status code.",
		Buckets:   []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"method", "code"})
	routerGRPCInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "monofs",
		Subsystem: "router",
		Name:      "grpc_inflight",
		Help:      "In-flight router gRPC handlers by method.",
	}, []string{"method"})
)

// MetricsUnaryInterceptor records router gRPC handler latency. It complements
// otelgrpc (which exports over OTLP) with a directly scrapeable Prometheus
// histogram, so /metrics has the same signal even when the collector is down.
func MetricsUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		routerGRPCInflight.WithLabelValues(info.FullMethod).Inc()
		defer routerGRPCInflight.WithLabelValues(info.FullMethod).Dec()
		resp, err := handler(ctx, req)
		routerGRPCDurationSeconds.WithLabelValues(info.FullMethod, status.Code(err).String()).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

func MetricsStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		routerGRPCInflight.WithLabelValues(info.FullMethod).Inc()
		defer routerGRPCInflight.WithLabelValues(info.FullMethod).Dec()
		err := handler(srv, stream)
		routerGRPCDurationSeconds.WithLabelValues(info.FullMethod, status.Code(err).String()).Observe(time.Since(start).Seconds())
		return err
	}
}
