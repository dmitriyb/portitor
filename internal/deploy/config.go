// Package deploy builds the gate image with Nix and replaces the gate container
// so that exactly one verified gate runs for a deploy config. See
// spec/delivery/arch_deploy.md.
package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var (
	// name is both a container name and an image path component, so it obeys
	// the stricter of the two: lowercase, separators only between alphanumerics.
	nameRE    = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	dockerRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)
	aliasRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	pkgRE     = regexp.MustCompile(`^[A-Za-z0-9_+-]+(\.[A-Za-z0-9_+-]+)*$`)
	revRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	nixHashRE = regexp.MustCompile(`^([0-9a-z]{52}|sha256-[A-Za-z0-9+/]{43}=)$`)
	hostIPRE  = regexp.MustCompile(`^[0-9a-fA-F.:]+$`)
)

// Network is a Docker network the gate joins, with its aliases there.
type Network struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// Port publishes the gate's SSH port 22 on the host.
type Port struct {
	HostIP   string `json:"host_ip"`
	HostPort int    `json:"host_port"`
}

// Pin is a nixpkgs snapshot.
type Pin struct {
	Rev    string `json:"rev"`
	SHA256 string `json:"sha256"`
}

// Image is the operator's declared part of the image.
type Image struct {
	Packages []string `json:"packages,omitempty"`
	Overlay  string   `json:"overlay,omitempty"`
	Pin      *Pin     `json:"pin,omitempty"`
}

// Config is a deploy config file.
type Config struct {
	Name           string    `json:"name"`
	Volume         string    `json:"volume"`
	HostKeyVolume  string    `json:"host_key_volume"`
	Networks       []Network `json:"networks,omitempty"`
	Ports          []Port    `json:"ports,omitempty"`
	ConfigDir      string    `json:"config_dir"`
	AuthorizedKeys []string  `json:"authorized_keys"`
	TokenCommand   []string  `json:"token_command"`
	HostKeyFile    string    `json:"host_key_file"`
	StartTimeout   string    `json:"start_timeout,omitempty"`
	StopTimeout    string    `json:"stop_timeout,omitempty"`
	Image          Image     `json:"image"`
	GateBin        string    `json:"gate_bin,omitempty"`

	startTimeout time.Duration
	stopTimeout  time.Duration
}

const (
	defaultStartTimeout = 2 * time.Minute
	defaultStopTimeout  = 30 * time.Second
)

// LoadConfig strictly decodes a deploy config, validates it, and resolves its
// relative path fields against the file's directory.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("deploy config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("deploy config %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("deploy config %s: trailing data after the JSON object", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("deploy config: %w", err)
	}
	if err := c.validate(filepath.Dir(abs)); err != nil {
		return nil, fmt.Errorf("deploy config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate(base string) error {
	if len(c.Name) < 2 || !nameRE.MatchString(c.Name) {
		return fmt.Errorf("name %q must be at least 2 characters matching %s", c.Name, nameRE)
	}
	for field, v := range map[string]string{"volume": c.Volume, "host_key_volume": c.HostKeyVolume} {
		if !dockerRE.MatchString(v) {
			return fmt.Errorf("%s %q must match %s", field, v, dockerRE)
		}
	}
	if c.Volume == c.HostKeyVolume {
		return fmt.Errorf("volume and host_key_volume must differ")
	}
	for _, n := range c.Networks {
		if !dockerRE.MatchString(n.Name) {
			return fmt.Errorf("network name %q must match %s", n.Name, dockerRE)
		}
		for _, a := range n.Aliases {
			if !aliasRE.MatchString(a) {
				return fmt.Errorf("network %s: alias %q must match %s", n.Name, a, aliasRE)
			}
		}
	}
	for _, p := range c.Ports {
		if !hostIPRE.MatchString(p.HostIP) || p.HostPort < 0 || p.HostPort > 65535 {
			return fmt.Errorf("port {host_ip %q, host_port %d} is not an IP and a port in 0..65535", p.HostIP, p.HostPort)
		}
	}
	if c.ConfigDir == "" || c.HostKeyFile == "" || len(c.AuthorizedKeys) == 0 {
		return fmt.Errorf("config_dir, host_key_file and authorized_keys are required")
	}
	if len(c.TokenCommand) == 0 || c.TokenCommand[0] == "" {
		return fmt.Errorf("token_command is required")
	}
	if c.GateBin != "" && !filepath.IsAbs(c.GateBin) {
		return fmt.Errorf("gate_bin %q must be an absolute path", c.GateBin)
	}
	for _, p := range c.Image.Packages {
		if !pkgRE.MatchString(p) {
			return fmt.Errorf("image package %q is not a nixpkgs attribute path", p)
		}
	}
	if pin := c.Image.Pin; pin != nil && (!revRE.MatchString(pin.Rev) || !nixHashRE.MatchString(pin.SHA256)) {
		return fmt.Errorf("image.pin needs a 40-hex rev and a nix sha256")
	}
	var err error
	if c.startTimeout, err = duration(c.StartTimeout, defaultStartTimeout); err != nil {
		return fmt.Errorf("start_timeout: %w", err)
	}
	if c.stopTimeout, err = duration(c.StopTimeout, defaultStopTimeout); err != nil {
		return fmt.Errorf("stop_timeout: %w", err)
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.ConfigDir = resolve(c.ConfigDir)
	c.HostKeyFile = resolve(c.HostKeyFile)
	c.Image.Overlay = resolve(c.Image.Overlay)
	for i := range c.AuthorizedKeys {
		c.AuthorizedKeys[i] = resolve(c.AuthorizedKeys[i])
	}
	return nil
}

func duration(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %s", s)
	}
	return d, nil
}
