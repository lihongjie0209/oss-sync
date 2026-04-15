package config

import (
	"fmt"
	"os"
	"strings"

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

	// Prefix scopes objects to a directory-like prefix.
	// For source it limits listing; for dest it prefixes uploaded object keys.
	Prefix string `yaml:"prefix"`

	// S3-specific options (used when provider = "s3").
	Region         string `yaml:"region"`
	ForcePathStyle bool   `yaml:"force_path_style"`

	// InsecureSkipVerify disables TLS certificate verification.
	// Use only for endpoints with self-signed or private-CA certificates.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type SyncConfig struct {
	Mode          string          `yaml:"mode"`            // full | incremental
	Concurrency   int             `yaml:"concurrency"`     // concurrent worker count
	RateLimitMbps float64         `yaml:"rate_limit_mbps"` // bandwidth cap MB/s, 0 = unlimited
	PageSize      int             `yaml:"page_size"`       // objects per list page
	RetryCount    int             `yaml:"retry_count"`     // retry attempts per file on transient failures
	DBPath        string          `yaml:"db_path"`         // sqlite file path
	Mappings      []PrefixMapping `yaml:"mappings"`
}

type Config struct {
	Source EndpointConfig `yaml:"source"`
	Dest   EndpointConfig `yaml:"dest"`
	Sync   SyncConfig     `yaml:"sync"`
}

type PrefixMapping struct {
	SourcePrefix string `yaml:"source_prefix"`
	DestPrefix   string `yaml:"dest_prefix"`
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

func normalizePrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
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
	if c.Sync.RetryCount <= 0 {
		c.Sync.RetryCount = 3
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
	c.Source.Prefix = normalizePrefix(c.Source.Prefix)
	c.Dest.Prefix = normalizePrefix(c.Dest.Prefix)
	for i := range c.Sync.Mappings {
		c.Sync.Mappings[i].SourcePrefix = normalizePrefix(c.Sync.Mappings[i].SourcePrefix)
		c.Sync.Mappings[i].DestPrefix = normalizePrefix(c.Sync.Mappings[i].DestPrefix)
	}
}

func (c *Config) PrefixMappings() []PrefixMapping {
	if len(c.Sync.Mappings) == 0 {
		return []PrefixMapping{{
			SourcePrefix: c.Source.Prefix,
			DestPrefix:   c.Dest.Prefix,
		}}
	}
	mappings := make([]PrefixMapping, len(c.Sync.Mappings))
	copy(mappings, c.Sync.Mappings)
	return mappings
}

func (c *Config) ScopeForMapping(mapping PrefixMapping) string {
	return fmt.Sprintf(
		"src:%s|%s|%s|%s=>dst:%s|%s|%s|%s",
		c.Source.Provider, c.Source.Endpoint, c.Source.Bucket, mapping.SourcePrefix,
		c.Dest.Provider, c.Dest.Endpoint, c.Dest.Bucket, mapping.DestPrefix,
	)
}

func (c *Config) Scopes() []string {
	mappings := c.PrefixMappings()
	scopes := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		scopes = append(scopes, c.ScopeForMapping(mapping))
	}
	return scopes
}
