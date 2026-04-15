package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadSingleMappingDefaultsAndScopes(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, `
source:
  provider: "oss"
  endpoint: "oss-cn-hangzhou.aliyuncs.com"
  access_key_id: "ak"
  access_key_secret: "sk"
  bucket: "src-bucket"
  prefix: "/images/raw"
dest:
  provider: "obs"
  endpoint: "obs.cn-north-4.myhuaweicloud.com"
  access_key_id: "ak"
  access_key_secret: "sk"
  bucket: "dst-bucket"
  prefix: "backup/2026"
sync:
  db_path: "./sync.db"
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Sync.Mode != "incremental" {
		t.Fatalf("expected default mode incremental, got %q", cfg.Sync.Mode)
	}
	if cfg.Sync.Concurrency != 10 {
		t.Fatalf("expected default concurrency 10, got %d", cfg.Sync.Concurrency)
	}
	if cfg.Sync.RetryCount != 3 {
		t.Fatalf("expected default retry count 3, got %d", cfg.Sync.RetryCount)
	}
	if cfg.Source.Prefix != "images/raw/" {
		t.Fatalf("expected normalized source prefix, got %q", cfg.Source.Prefix)
	}
	if cfg.Dest.Prefix != "backup/2026/" {
		t.Fatalf("expected normalized dest prefix, got %q", cfg.Dest.Prefix)
	}

	mappings := cfg.PrefixMappings()
	if len(mappings) != 1 {
		t.Fatalf("expected 1 fallback mapping, got %d", len(mappings))
	}
	if mappings[0].SourcePrefix != "images/raw/" || mappings[0].DestPrefix != "backup/2026/" {
		t.Fatalf("unexpected fallback mapping: %+v", mappings[0])
	}

	scopes := cfg.Scopes()
	if len(scopes) != 1 {
		t.Fatalf("expected 1 scope, got %d", len(scopes))
	}
	wantScope := "src:oss|oss-cn-hangzhou.aliyuncs.com|src-bucket|images/raw/=>dst:obs|obs.cn-north-4.myhuaweicloud.com|dst-bucket|backup/2026/"
	if scopes[0] != wantScope {
		t.Fatalf("unexpected scope: %q", scopes[0])
	}
}

func TestLoadMappingsOverrideTopLevelPrefixes(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, `
source:
  provider: "oss"
  endpoint: "src-endpoint"
  access_key_id: "ak"
  access_key_secret: "sk"
  bucket: "src-bucket"
  prefix: "ignored-source"
dest:
  provider: "obs"
  endpoint: "dst-endpoint"
  access_key_id: "ak"
  access_key_secret: "sk"
  bucket: "dst-bucket"
  prefix: "ignored-dest"
sync:
  mappings:
    - source_prefix: "images/raw"
      dest_prefix: "backup/raw"
    - source_prefix: ""
      dest_prefix: ""
`))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	mappings := cfg.PrefixMappings()
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(mappings))
	}
	if mappings[0].SourcePrefix != "images/raw/" || mappings[0].DestPrefix != "backup/raw/" {
		t.Fatalf("unexpected first mapping: %+v", mappings[0])
	}
	if mappings[1].SourcePrefix != "" || mappings[1].DestPrefix != "" {
		t.Fatalf("unexpected root mapping normalization: %+v", mappings[1])
	}

	scopes := cfg.Scopes()
	if len(scopes) != 2 {
		t.Fatalf("expected 2 scopes, got %d", len(scopes))
	}
	if scopes[0] == scopes[1] {
		t.Fatalf("expected distinct scopes per mapping, got %v", scopes)
	}
}

func TestLoadRejectsInvalidMode(t *testing.T) {
	_, err := Load(writeConfigFile(t, `
source:
  provider: "oss"
  endpoint: "src-endpoint"
  access_key_id: "ak"
  access_key_secret: "sk"
  bucket: "src-bucket"
dest:
  provider: "obs"
  endpoint: "dst-endpoint"
  access_key_id: "ak"
  access_key_secret: "sk"
  bucket: "dst-bucket"
sync:
  mode: "delta"
`))
	if err == nil {
		t.Fatal("expected invalid mode error")
	}
}
