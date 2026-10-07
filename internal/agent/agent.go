package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/heainframework/heain-sdk/heain"
)

// Headers the agent sends to the app with every callback.
const (
	HeaderRequest = "X-Heain-Request" // ties the app's own calls back to this job or request
	HeaderCaller  = "X-Heain-Caller"  // the calling app's certificate identity
	HeaderUser    = "X-Heain-User"    // the person a gateway vouched for, if any
	HeaderRoles   = "X-Heain-User-Roles"
	HeaderTicket  = "X-Heain-Ticket"
)

// Agent runs one app's sidecar.
type Agent struct {
	App   *heain.App
	Cfg   Config
	Token string

	client *http.Client
	mu     sync.Mutex
	inflt  map[string]context.Context // request id -> the job's or request's context
}

// New prepares an agent for a registered app; the token is written to
// cfg.TokenFile (or <stateDir>/agent.token).
func New(app *heain.App, cfg Config, stateDir string) (*Agent, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	a := &Agent{App: app, Cfg: cfg, Token: hex.EncodeToString(b), inflt: map[string]context.Context{},
		client: &http.Client{Transport: &http.Transport{Proxy: nil}}}
	if a.Cfg.TokenFile == "" {
		a.Cfg.TokenFile = filepath.Join(stateDir, "agent.token")
	}
	if err := os.MkdirAll(filepath.Dir(a.Cfg.TokenFile), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(a.Cfg.TokenFile, []byte(a.Token), 0o600); err != nil {
		return nil, fmt.Errorf("heain-agent: writing the token: %w", err)
	}
	for _, c := range app.Manifest.Capabilities {
		if c.Execution == "job" {
			if _, ok := cfg.Jobs[c.Name]; !ok && len(cfg.Jobs) > 0 {
				log.Printf("heain-agent: job capability %s has no callback in the config: its jobs are not claimed", c.Name)
			}
		}
	}
	return a, nil
}

// track registers ctx under a new request id until done is called.
func (a *Agent) track(ctx context.Context, id string) (string, func()) {
	if id == "" {
		id = heain.NewID()
	}
	a.mu.Lock()
	a.inflt[id] = ctx
	a.mu.Unlock()
	return id, func() {
		a.mu.Lock()
		delete(a.inflt, id)
		a.mu.Unlock()
	}
}

// ctxFor is the context an app call runs in: the job's or request's when the
// app names it (X-Heain-Request), else a fresh one with the trace it sends.
func (a *Agent) ctxFor(r *http.Request) (context.Context, error) {
	if id := r.Header.Get(HeaderRequest); id != "" {
		a.mu.Lock()
		ctx, ok := a.inflt[id]
		a.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%s %q is not a job or request in progress", HeaderRequest, id)
		}
		return ctx, nil
	}
	ctx := r.Context()
	if t := r.Header.Get(heain.HeaderTrace); t != "" {
		ctx = heain.WithTrace(ctx, t, r.Header.Get(heain.HeaderLane))
	}
	return ctx, nil
}

// Run serves the local API, runs the jobs and serves the direct endpoints
// until ctx ends or the app (if started here) exits.
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l, err := net.Listen("tcp", a.Cfg.Listen)
	if err != nil {
		return err
	}
	local := &http.Server{Handler: a.LocalHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = local.Close() }()
	errc := make(chan error, 8)
	go func() {
		if err := local.Serve(l); err != nil && ctx.Err() == nil {
			errc <- fmt.Errorf("local API: %w", err)
		}
	}()
	log.Printf("heain-agent: local API on http://%s (token in %s)", l.Addr(), a.Cfg.TokenFile)
	childDone := make(chan struct{})
	if len(a.Cfg.Command) > 0 {
		go func() {
			defer close(childDone)
			if err := a.runChild(ctx, "http://"+l.Addr().String()); err != nil {
				errc <- err
			}
		}()
	} else {
		close(childDone)
	}
	// on the way out, the app (if started here) is stopped first
	defer func() {
		cancel()
		select {
		case <-childDone:
		case <-time.After(15 * time.Second):
		}
	}()
	if err := a.waitReady(ctx); err != nil {
		return err
	}
	go func() {
		if a.App.WaitActive(ctx) == nil {
			log.Printf("heain-agent: %s.%s: active", a.App.Manifest.App.ID, a.App.Instance)
		}
	}()
	if wk := a.worker(); wk != nil {
		go func() {
			if a.App.WaitActive(ctx) == nil {
				_ = wk.Run(ctx)
			}
		}()
	}
	if len(a.App.Manifest.Endpoints) > 0 {
		srv, err := a.endpoints()
		if err != nil {
			return err
		}
		el, err := net.Listen("tcp", heain.Listen(orDefault(a.Cfg.EndpointListen, "0.0.0.0:19443")))
		if err != nil {
			return err
		}
		log.Printf("heain-agent: direct endpoints on %s (mTLS) -> %s", el.Addr(), a.Cfg.AppURL)
		go func() {
			if err := srv.Serve(ctx, el); err != nil && ctx.Err() == nil {
				errc <- fmt.Errorf("endpoints: %w", err)
			}
		}()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (a *Agent) waitReady(ctx context.Context) error {
	if a.Cfg.Ready == "" {
		return nil
	}
	end := time.Now().Add(60 * time.Second)
	for time.Now().Before(end) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.Cfg.AppURL+a.Cfg.Ready, nil)
		if resp, err := a.client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("heain-agent: the app did not answer %s%s within 60 s", a.Cfg.AppURL, a.Cfg.Ready)
}

// runChild starts the app and returns when it exits; when ctx ends it asks
// the app to stop (SIGTERM, then SIGKILL after 10 s).
func (a *Agent) runChild(ctx context.Context, localURL string) error {
	cmd := exec.Command(a.Cfg.Command[0], a.Cfg.Command[1:]...)
	cmd.Env = append(os.Environ(), "HEAIN_AGENT_URL="+localURL, "HEAIN_AGENT_TOKEN="+a.Token, "HEAIN_AGENT_TOKEN_FILE="+a.Cfg.TokenFile)
	if a.Cfg.AppURL != "" {
		cmd.Env = append(cmd.Env, "HEAIN_APP_URL="+a.Cfg.AppURL)
		if u, err := url.Parse(a.Cfg.AppURL); err == nil {
			cmd.Env = append(cmd.Env, "HEAIN_APP_PORT="+u.Port())
		}
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("heain-agent: starting the app: %w", err)
	}
	log.Printf("heain-agent: started the app %v (pid %d)", a.Cfg.Command, cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return fmt.Errorf("heain-agent: the app exited: %v", err)
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return nil
	}
}

// ---------------- jobs ----------------

// jobReply is what the app answers to a job callback.
type jobReply struct {
	Output    []byte     `json:"output_b64"`
	Reasoning []decision `json:"reasoning"`
	Error     string     `json:"error"`
	Permanent bool       `json:"permanent"`
}

func (a *Agent) worker() *heain.Worker {
	if len(a.Cfg.Jobs) == 0 {
		return nil
	}
	wk := a.App.NewWorker()
	wk.Concurrency = a.Cfg.Concurrency
	n := 0
	for capName, path := range a.Cfg.Jobs {
		path := path
		if err := wk.Handle(capName, func(ctx context.Context, j *heain.Job) ([]byte, error) {
			return a.runJob(ctx, path, j)
		}); err != nil {
			log.Printf("heain-agent: %v", err)
			continue
		}
		n++
	}
	if n == 0 {
		return nil
	}
	return wk
}

func (a *Agent) runJob(ctx context.Context, path string, j *heain.Job) ([]byte, error) {
	id, done := a.track(ctx, j.TicketID)
	defer done()
	body, _ := json.Marshal(map[string]any{"ticket_id": j.TicketID, "capability": j.Capability, "trace_id": heain.TraceID(ctx),
		"lane": heain.Lane(ctx), "lease_expires_at": j.LeaseExpiresAt, "payload_b64": j.Payload, "request": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.AppURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderRequest, id)
	req.Header.Set(HeaderTicket, j.TicketID)
	req.Header.Set(heain.HeaderTrace, heain.TraceID(ctx))
	if l := heain.Lane(ctx); l != "" {
		req.Header.Set(heain.HeaderLane, l)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the app did not take job %s: %w", j.TicketID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	var rep jobReply
	_ = json.Unmarshal(raw, &rep)
	for i := range rep.Reasoning {
		d, err := rep.Reasoning[i].toSDK(j.Capability)
		if err != nil {
			return nil, heain.Permanent(fmt.Errorf("reasoning %d: %w", i, err))
		}
		if _, err := a.App.Reason(ctx, d); err != nil {
			return nil, fmt.Errorf("reasoning record: %w", err)
		}
	}
	if resp.StatusCode >= 300 {
		msg := rep.Error
		if msg == "" {
			msg = fmt.Sprintf("the app answered %d", resp.StatusCode)
		}
		if rep.Permanent || resp.StatusCode == http.StatusUnprocessableEntity {
			return nil, heain.Permanent(errors.New(msg))
		}
		return nil, errors.New(msg)
	}
	return rep.Output, nil
}

// ---------------- direct endpoints ----------------

func (a *Agent) endpoints() (*heain.Server, error) {
	srv := a.App.NewServer()
	for _, ep := range a.App.Manifest.Endpoints {
		pattern := strings.ToUpper(ep.Method) + " " + ep.Path
		target, mapped := a.Cfg.Endpoints[pattern]
		if err := srv.Handle(pattern, a.forward(target, mapped)); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

// forward sends a request the SDK has admitted (mTLS, lanes, user, offline
// rules) to the app, and its answer back; the SDK audits it and checks the
// reasoning record.
func (a *Agent) forward(target string, mapped bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, done := a.track(r.Context(), "")
		defer done()
		path := r.URL.Path
		if mapped {
			path = target
		}
		u := a.Cfg.AppURL + path
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, u, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for _, h := range []string{"Content-Type", "Accept", "Content-Length"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		req.ContentLength = r.ContentLength
		req.Header.Set(HeaderRequest, id)
		req.Header.Set(heain.HeaderTrace, heain.TraceID(r.Context()))
		if l := heain.Lane(r.Context()); l != "" {
			req.Header.Set(heain.HeaderLane, l)
		}
		if c := heain.Caller(r.Context()); c != "" {
			req.Header.Set(HeaderCaller, c)
		}
		if usr := heain.UserOf(r.Context()); usr != nil {
			req.Header.Set(HeaderUser, usr.ID)
			req.Header.Set(HeaderRoles, strings.Join(usr.Roles, ","))
		}
		resp, err := a.client.Do(req)
		if err != nil {
			http.Error(w, "the app did not answer: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			if k == "Content-Length" || k == "Connection" || k == "Transfer-Encoding" {
				continue
			}
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}
