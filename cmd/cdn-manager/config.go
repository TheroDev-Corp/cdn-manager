package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/TheroDev-Corp/cdn-manager/internal/auth"
	"github.com/TheroDev-Corp/cdn-manager/internal/service"
	"github.com/TheroDev-Corp/cdn-manager/internal/storage"
)

type authConfig struct {
	PublicURL              string
	OIDCIssuer             string
	OIDCClientID           string
	OIDCClientSecretEnv    string
	MCPIssuer              string
	MCPAudience            string
	MCPScopes              []string
	MCPBrokerKeyEnv        string
	SessionTTL             string
	AllowInsecureLocalhost bool
}

type config struct {
	Auth    authConfig
	Sources []service.SourceConfig
}

func readConfig(path string) (config, error) {
	var cfg config
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("trailing content in configuration")
	}
	if len(cfg.Sources) == 0 {
		return cfg, errors.New("configure at least one storage source")
	}
	return cfg, nil
}

func (cfg authConfig) provider() (auth.Config, error) {
	var ttl time.Duration
	var brokerKey []byte
	if cfg.MCPBrokerKeyEnv != "" {
		var err error
		brokerKey, err = base64.StdEncoding.DecodeString(os.Getenv(cfg.MCPBrokerKeyEnv))
		if err != nil || len(brokerKey) != 32 {
			return auth.Config{}, errors.New("auth.mcpBrokerKeyEnv must name a base64-encoded 32-byte key")
		}
	}
	if cfg.SessionTTL != "" {
		var err error
		ttl, err = time.ParseDuration(cfg.SessionTTL)
		if err != nil {
			return auth.Config{}, errors.New("auth.sessionTtl must be a duration, for example 8h")
		}
	}
	return auth.Config{
		PublicURL: cfg.PublicURL, OIDCIssuer: cfg.OIDCIssuer, OIDCClientID: cfg.OIDCClientID,
		OIDCClientSecret: os.Getenv(cfg.OIDCClientSecretEnv), MCPIssuer: cfg.MCPIssuer, MCPAudience: cfg.MCPAudience,
		MCPScopes: cfg.MCPScopes, MCPBrokerKey: brokerKey, SessionTTL: ttl, AllowInsecureLocalhost: cfg.AllowInsecureLocalhost,
	}, nil
}

func (cfg config) storage() ([]storage.SourceConfig, error) {
	sources := make([]storage.SourceConfig, 0, len(cfg.Sources))
	for _, input := range cfg.Sources {
		source := input.SourceConfig
		if input.AccessKeyEnv == "" || input.SecretKeyEnv == "" {
			return nil, fmt.Errorf("source %q needs credential environment references", source.ID)
		}
		if len(input.Roles.Admin)+len(input.Roles.Editor)+len(input.Roles.Viewer) == 0 {
			return nil, fmt.Errorf("source %q needs at least one role mapping", source.ID)
		}
		source.AccessKey = os.Getenv(input.AccessKeyEnv)
		source.SecretKey = os.Getenv(input.SecretKeyEnv)
		source.SessionToken = os.Getenv(input.SessionTokenEnv)
		sources = append(sources, source)
	}
	return sources, nil
}

func setting(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
