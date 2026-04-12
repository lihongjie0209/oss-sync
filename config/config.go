package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// EndpointConfig is the common config for any storage provider endpoint.
type EndpointConfig struct {
	// Provider selects the storage backend: "oss" | "obs" | "s3"
	// "s3" works with any S3-compatible service (MinIO, AWS S3, R2, etc.)
	Provider string `yaml:"provider"`

	Endpoint        string `yaml:"endpoint"`
	AccessKeyID     string `yaml:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret"`
	Bucket          string `yaml:"bucket"`

	// Prefix is only used for the source (objects to sync from).
	Prefix string `yaml:"prefix"`

	// S3-specific options (used when provider = "s3").
	Region         string `yaml:"region"`
	ForcePathStyle bool   `yaml:"force_path_style"`
}

type SyncConfig struct {
	Mode          string  `yaml:"mode"`            // full | incremental
	Concurrency   int     `yaml:"concurrency"`     // concurrent worker count
	RateLimitMbps float64 `yaml:"rate_limit_mbps"` // bandwidth cap MB/s, 0 = unlimited
	PageSize      int     `yaml:"page_size"`       // objects per list page
	DBPath        string  `yaml:"db_path"`         // sqlite file path
}

type Config struct {
	Source EndpointConfig `yaml:"source"`
	Dest   EndpointConfig `yaml:"dest"`
	Sync   SyncConfig     `yaml:"sync"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	cfg.setDefaults()
	return cfg, nil
}

func (c *Config) validate() error {
	for side, ep := range map[string]*EndpointConfig{"source": &c.Source, "dest": &c.Dest} {
		if ep.Provider == "" {
			return fmt.Errorf("%s.provider is required (oss | obs | s3)", side)
		}
		switch ep.Provider {
		case "oss", "obs", "s3":
		default:
			return fmt.Errorf("%s.provider %q is unknown (oss | obs | s3)", side, ep.Provider)
		}
		if ep.Endpoint == "" {
			return fmt.Errorf("%s.endpoint is required", side)
		}
		if ep.Bucket == "" {
			return fmt.Errorf("%s.bucket is required", side)
		}
	}
	if c.Sync.Mode != "" && c.Sync.Mode != "full" && c.Sync.Mode != "incremental" {
		return fmt.Errorf("sync.mode must be 'full' or 'incremental'")
	}
	return nil
}

func (c *Config) setDefaults() {
	if c.Sync.Mode == "" {
		c.Sync.Mode = "incremental"
	}
	if c.Sync.Concurrency <= 0 {
		c.Sync.Concurrency = 10
	}
	if c.Sync.PageSize <= 0 {
		c.Sync.PageSize = 1000
	}
	if c.Sync.DBPath == "" {
		c.Sync.DBPath = "./sync.db"
	}
	if c.Source.Region == "" {
		c.Source.Region = "us-east-1"
	}
	if c.Dest.Region == "" {
		c.Dest.Region = "us-east-1"
	}
}

