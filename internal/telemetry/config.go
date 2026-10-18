package telemetry

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultServiceName    = "monofs"
	defaultMetricInterval = 15 * time.Second
)

type Config struct {
	Endpoint       string
	ServiceName    string
	Component      string
	InstanceID     string
	Insecure       bool
	MetricInterval time.Duration
}

func LoadConfig(component string) (Config, error) {
	cfg := Config{
		Endpoint:  envFirst("OTEL_EXPORTER_OTLP_ENDPOINT", "MONOFS_OTEL_ENDPOINT"),
		Component: strings.TrimSpace(component),
	}
	if cfg.Component == "" {
		cfg.Component = filepath.Base(os.Args[0])
	}
	cfg.InstanceID = strings.TrimSpace(os.Getenv("HOSTNAME"))
	if cfg.InstanceID == "" {
		cfg.InstanceID = cfg.Component
	}
	if cfg.Endpoint == "" {
		return cfg, nil
	}
	cfg.ServiceName = envFirst("OTEL_SERVICE_NAME", "MONOFS_OTEL_SERVICE_NAME")
	if cfg.ServiceName == "" {
		cfg.ServiceName = defaultServiceName
	}
	insecure := envFirst("OTEL_EXPORTER_OTLP_INSECURE", "MONOFS_OTEL_INSECURE")
	if insecure == "" {
		cfg.Insecure = true
	} else {
		value, err := strconv.ParseBool(insecure)
		if err != nil {
			return Config{}, fmt.Errorf("parse MONOFS_OTEL_INSECURE: %w", err)
		}
		cfg.Insecure = value
	}
	metricInterval := strings.TrimSpace(os.Getenv("MONOFS_OTEL_METRIC_INTERVAL"))
	if metricInterval == "" {
		cfg.MetricInterval = defaultMetricInterval
	} else {
		value, err := time.ParseDuration(metricInterval)
		if err != nil {
			return Config{}, fmt.Errorf("parse MONOFS_OTEL_METRIC_INTERVAL: %w", err)
		}
		cfg.MetricInterval = value
	}
	return cfg, nil
}

func envFirst(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
