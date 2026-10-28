package config

import (
	"bytes"
	"fmt"
	"os"

	"github.com/a8m/envsubst"
	"gopkg.in/yaml.v3"
)

// Load reads and validates a config file, applying schema-level defaults.
// CLI profile defaults and CLI-specific validation are applied separately by
// cli.Prepare.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Expand environment references before decoding: $NAME, ${NAME},
	// ${NAME:-default} and ${NAME:=default}; "$$" escapes a literal dollar.
	// A reference must name a set variable unless it carries a non-empty
	// default, so a missing value fails the load; a name may not start with a
	// digit, so "$1" stays literal.
	expanded, err := envsubst.BytesRestrictedNoDigit(b, true, false, true)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// MustLoad is Load that panics on error.
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(err)
	}

	return cfg
}
