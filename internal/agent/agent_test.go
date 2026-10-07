package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heainframework/heain-sdk/heain"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "heain-agent.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigDefaults(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:7600" || c.Concurrency != 1 {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestConfigRefuses(t *testing.T) {
	cases := map[string]string{
		"non-loopback listen":   `{"listen":"0.0.0.0:7600"}`,
		"public listen":         `{"listen":"10.1.2.3:7600"}`,
		"https app_url":         `{"app_url":"https://127.0.0.1:8080"}`,
		"remote app_url":        `{"app_url":"http://192.168.1.5:8080"}`,
		"port 0 without cmd":    `{"app_url":"http://127.0.0.1:0"}`,
		"jobs without app_url":  `{"jobs":{"x.y":"/jobs/x"}}`,
		"job path not absolute": `{"app_url":"http://127.0.0.1:8080","jobs":{"x.y":"jobs/x"}}`,
		"endpoint path":         `{"app_url":"http://127.0.0.1:8080","endpoints":{"GET /a":"a"}}`,
		"ready path":            `{"app_url":"http://127.0.0.1:8080","ready":"ready"}`,
		"unknown field":         `{"listn":"127.0.0.1:1"}`,
	}
	for name, body := range cases {
		if _, err := LoadConfig(writeCfg(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigPort0PicksAPort(t *testing.T) {
	c, err := LoadConfig(writeCfg(t, `{"listen":"localhost:0","app_url":"http://127.0.0.1:0","command":["true"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(c.AppURL, ":0") || !strings.HasPrefix(c.AppURL, "http://127.0.0.1:") {
		t.Fatalf("app_url %q", c.AppURL)
	}
}

func TestDecisionToSDK(t *testing.T) {
	var d decision
	if err := json.Unmarshal([]byte(`{"decision":"allow","input_b64":"aGk=","confidence":0.9,"summary":"s"}`), &d); err != nil {
		t.Fatal(err)
	}
	got, err := d.toSDK("ref.score")
	if err != nil {
		t.Fatal(err)
	}
	if got.Capability != "ref.score" || string(got.Input) != "hi" || got.Confidence == nil || *got.Confidence != 0.9 || got.Summary != "s" {
		t.Fatalf("mapped %+v", got)
	}
	if _, err := (decision{Decision: "x"}).toSDK(""); err == nil {
		t.Fatal("no capability accepted")
	}
	if _, err := (decision{Capability: "c"}).toSDK(""); err == nil {
		t.Fatal("no decision accepted")
	}
}

func TestFromSDK(t *testing.T) {
	e := fromSDK(&heain.CallError{Status: 403, Code: "denied", Body: "no"}).(*apiErr)
	if e.status != 403 || e.code != "denied" {
		t.Fatalf("call error: %+v", e)
	}
	e = fromSDK(bad("x")).(*apiErr)
	if e.status != 400 || e.code != "bad_request" {
		t.Fatalf("api error: %+v", e)
	}
	e = fromSDK(errors.New("boom")).(*apiErr)
	if e.status != 502 || e.code != "core_error" {
		t.Fatalf("other: %+v", e)
	}
}

func TestLocalAPIToken(t *testing.T) {
	a := &Agent{Token: "secret", inflt: map[string]context.Context{}}
	srv := httptest.NewServer(a.LocalHandler())
	defer srv.Close()
	do := func(tok, reqID string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/mode", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if reqID != "" {
			req.Header.Set(HeaderRequest, reqID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error.Code
	}
	if s, c := do("", ""); s != 401 || c != "unauthenticated" {
		t.Fatalf("no token: %d %s", s, c)
	}
	if s, c := do("wrong", ""); s != 401 || c != "unauthenticated" {
		t.Fatalf("wrong token: %d %s", s, c)
	}
	if s, c := do("secret", "no-such-request"); s != 400 || c != "unknown_request" {
		t.Fatalf("unknown request: %d %s", s, c)
	}
}

func TestTrackBindsRequest(t *testing.T) {
	a := &Agent{inflt: map[string]context.Context{}}
	type k struct{}
	ctx := context.WithValue(context.Background(), k{}, "job")
	id, done := a.track(ctx, "")
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(HeaderRequest, id)
	got, err := a.ctxFor(r)
	if err != nil || got.Value(k{}) != "job" {
		t.Fatalf("bound context: %v", err)
	}
	done()
	if _, err := a.ctxFor(r); err == nil {
		t.Fatal("finished request still bound")
	}
}

func TestNewWritesToken0600(t *testing.T) {
	dir := t.TempDir()
	a, err := New(&heain.App{}, Config{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "agent.token"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || len(a.Token) != 64 {
		t.Fatalf("mode %v, token len %d", st.Mode().Perm(), len(a.Token))
	}
	b, _ := os.ReadFile(filepath.Join(dir, "agent.token"))
	if string(b) != a.Token {
		t.Fatal("token file differs")
	}
}
