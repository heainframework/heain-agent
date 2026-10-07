// Package agent is heain-agent: the sidecar that does heain-sdk's job for an
// app written in any other language (Step 5, author decisions 2026-10-08,
// project doc claude/decision-2026-10-08-heain-agent.md).
//
//   - The agent registers with core, keeps the registration alive and holds
//     the app certificate, exactly as heain-sdk does (it is heain-sdk inside).
//   - The app talks to the agent over HTTP/JSON on loopback only, with a
//     per-start token (Authorization: Bearer).
//   - Inbound work reaches the app as callbacks: the agent claims each job
//     and POSTs it to the app; it terminates the mTLS of every declared
//     direct endpoint and forwards the request to the app.
//   - The SDK's rules hold: formal processes are audited automatically, and a
//     capability with ai.used must give a reasoning record for every job and
//     every answer, or it fails.
package agent

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Config is the agent's configuration (file named by HEAIN_AGENT_CONFIG).
type Config struct {
	// Listen is the local API's address; it must be a loopback address
	// (default 127.0.0.1:7600; port 0 picks a free one, given to a Command
	// in HEAIN_AGENT_URL).
	Listen string `json:"listen"`
	// TokenFile receives the local API token at start (mode 0600; default
	// <HEAIN_STATE_DIR>/agent.token). A new token is made on every start.
	TokenFile string `json:"token_file"`
	// AppURL is the app's own loopback base URL, for callbacks. With port 0
	// (e.g. http://127.0.0.1:0) and a Command, the agent picks a free port
	// and gives it to the app in HEAIN_APP_PORT / HEAIN_APP_URL, so several
	// instances can run on one machine.
	AppURL string `json:"app_url"`
	// Jobs maps each execution: job capability the app handles to the path
	// (under AppURL) the agent POSTs a claimed job to.
	Jobs map[string]string `json:"jobs"`
	// Endpoints maps a declared endpoint ("METHOD /path" as in the manifest)
	// to the path under AppURL it is forwarded to. Unlisted endpoints are
	// forwarded to the same path.
	Endpoints map[string]string `json:"endpoints"`
	// EndpointListen is where the mTLS direct endpoints are served
	// (default: HEAIN_LISTEN, else 0.0.0.0:19443).
	EndpointListen string `json:"endpoint_listen"`
	// Concurrency is how many jobs run at once (default 1).
	Concurrency int `json:"concurrency"`
	// Command, when set, is the app to start as the agent's child, with
	// HEAIN_AGENT_URL and HEAIN_AGENT_TOKEN set. When it exits the agent
	// stops; when the agent stops it stops the app.
	Command []string `json:"command"`
	// Ready, when set, is a path under AppURL that must answer 2xx before
	// the agent takes jobs or serves endpoints.
	Ready string `json:"ready"`
}

// LoadConfig reads and checks the configuration file.
func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("heain-agent: reading config: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("heain-agent: config %s: %w", path, err)
	}
	return c, c.check()
}

func (c *Config) check() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7600"
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || !loopback(host) {
		return fmt.Errorf("heain-agent: listen %q must be a loopback address (the local API carries no TLS)", c.Listen)
	}
	if c.AppURL != "" {
		u, err := url.Parse(c.AppURL)
		if err != nil || u.Scheme != "http" || !loopback(u.Hostname()) {
			return fmt.Errorf("heain-agent: app_url %q must be http on a loopback address", c.AppURL)
		}
	}
	if u, err := url.Parse(c.AppURL); err == nil && u.Port() == "0" {
		if len(c.Command) == 0 {
			return fmt.Errorf("heain-agent: app_url port 0 needs a command (the agent tells the app its port)")
		}
		l, err := net.Listen("tcp", net.JoinHostPort(u.Hostname(), "0"))
		if err != nil {
			return err
		}
		c.AppURL = "http://" + l.Addr().String()
		l.Close()
	}
	if (len(c.Jobs) > 0 || len(c.Endpoints) > 0) && c.AppURL == "" {
		return fmt.Errorf("heain-agent: app_url is required for jobs and endpoints")
	}
	for k, v := range c.Jobs {
		if !strings.HasPrefix(v, "/") {
			return fmt.Errorf("heain-agent: jobs[%s] must be a path starting with /", k)
		}
	}
	for k, v := range c.Endpoints {
		if !strings.HasPrefix(v, "/") {
			return fmt.Errorf("heain-agent: endpoints[%s] must be a path starting with /", k)
		}
	}
	if c.Ready != "" && !strings.HasPrefix(c.Ready, "/") {
		return fmt.Errorf("heain-agent: ready must be a path starting with /")
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
