package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Peer struct {
	Token string   `json:"token"`
	Name  string   `json:"name,omitempty"`
	Nodes []string `json:"nodes,omitempty"`
}
type Model struct {
	URL   string `json:"url"`
	Model string `json:"model"`
	Key   string `json:"key,omitempty"`
}

// Pi selects the installed SDK runtime used by Hub, independently of task agents.
type Pi struct {
	Node    string `json:"node,omitempty"`
	Package string `json:"package,omitempty"`
}
type Agent struct {
	Executable string            `json:"executable"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}
type Config struct {
	StateDir    string            `json:"state_dir"`
	Listen      string            `json:"listen,omitempty"`
	Cert        string            `json:"tls_cert,omitempty"`
	Key         string            `json:"tls_key,omitempty"`
	Workers     map[string]Peer   `json:"workers,omitempty"`
	Channels    map[string]Peer   `json:"channels,omitempty"`
	Model       Model             `json:"model,omitempty"`
	Coordinator Pi                `json:"coordinator,omitempty"`
	WeCom       *WeCom            `json:"wecom,omitempty"`
	Hub         string            `json:"hub,omitempty"`
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name,omitempty"`
	Token       string            `json:"token,omitempty"`
	CA          string            `json:"ca,omitempty"`
	Backend     string            `json:"backend,omitempty"`
	Mux         string            `json:"mux,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Bridge      string            `json:"bridge,omitempty"`
	Workspaces  map[string]string `json:"workspaces,omitempty"`
	Agents      map[string]Agent  `json:"agents,omitempty"`
	MaxRunning  int               `json:"max_running,omitempty"`
	Spool       string            `json:"spool,omitempty"`
	Shell       Shell             `json:"shell,omitempty"`
}

func Secret(s string) (string, error) {
	if strings.HasPrefix(s, "env:") {
		s = os.Getenv(strings.TrimPrefix(s, "env:"))
		if s == "" {
			return "", errors.New("required credential environment variable is empty")
		}
	}
	return s, nil
}
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	base, _ := filepath.Abs(filepath.Dir(path))
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.StateDir = abs(c.StateDir)
	c.Cert = abs(c.Cert)
	c.Key = abs(c.Key)
	c.CA = abs(c.CA)
	c.Bridge = abs(c.Bridge)
	c.Spool = abs(c.Spool)
	c.Coordinator.Package = abs(c.Coordinator.Package)
	if strings.ContainsAny(c.Shell.Executable, `/\`) {
		c.Shell.Executable = abs(c.Shell.Executable)
	}
	if strings.ContainsAny(c.Coordinator.Node, `/\`) {
		c.Coordinator.Node = abs(c.Coordinator.Node)
	}
	if c.WeCom != nil && c.WeCom.Helper != "" {
		c.WeCom.Helper = abs(c.WeCom.Helper)
	}
	for k, p := range c.Workspaces {
		c.Workspaces[k] = abs(p)
	}
	if c.StateDir == "" {
		return c, errors.New("state_dir is required")
	}
	if c.MaxRunning == 0 {
		c.MaxRunning = 4
	}
	if c.MaxRunning < 1 {
		return c, errors.New("max_running must be positive")
	}
	if c.Backend == "" {
		c.Backend = "tmux"
	}
	if c.Mux == "" {
		c.Mux = c.Backend
	}
	if c.Namespace == "" {
		c.Namespace = "relaydock"
	}
	c.Token, err = Secret(c.Token)
	if err != nil {
		return c, err
	}
	c.Model.Key, err = Secret(c.Model.Key)
	if err != nil {
		return c, err
	}
	for id, p := range c.Workers {
		p.Token, err = Secret(p.Token)
		if err != nil {
			return c, err
		}
		c.Workers[id] = p
	}
	for id, p := range c.Channels {
		p.Token, err = Secret(p.Token)
		if err != nil {
			return c, err
		}
		c.Channels[id] = p
	}
	return c, nil
}
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func ClientTLS(c Config) (*tls.Config, error) {
	u, err := url.Parse(c.Hub)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "wss" && !(u.Scheme == "ws" && Loopback(u.Hostname())) {
		return nil, errors.New("hub must use wss; ws is allowed only on loopback")
	}
	t := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CA != "" {
		b, err := os.ReadFile(c.CA)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid CA certificate")
		}
		t.RootCAs = pool
	}
	return t, nil
}
func (c Config) CheckHub() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return err
	}
	if (c.Cert == "") != (c.Key == "") {
		return errors.New("tls_cert and tls_key must be provided together")
	}
	if c.Cert == "" && !Loopback(host) {
		return errors.New("non-loopback hub listener requires TLS")
	}
	seen := map[string]bool{}
	for _, peers := range []map[string]Peer{c.Workers, c.Channels} {
		for id, p := range peers {
			if id == "" || len(p.Token) < 24 {
				return fmt.Errorf("peer %q needs a unique token of at least 24 characters", id)
			}
			if seen[p.Token] {
				return errors.New("peer tokens must be unique")
			}
			seen[p.Token] = true
		}
	}
	return nil
}
