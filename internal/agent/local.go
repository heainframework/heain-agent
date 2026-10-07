package agent

// The local API (loopback, Authorization: Bearer <token>): heain-sdk's
// calls, as HTTP/JSON for an app in any language. A call made while
// handling a job or a request names it with X-Heain-Request, so it runs in
// that job's or request's trace, lane and reasoning-record context.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/heainframework/heain-sdk/heain"
)

// decision is a reasoning record's input (spec 04), as JSON.
type decision struct {
	Capability     string             `json:"capability"`
	Input          []byte             `json:"input_b64"` // hashed here; never leaves the agent
	InputDataClass string             `json:"input_data_class"`
	InputRef       string             `json:"input_ref"`
	Decision       string             `json:"decision"`
	Value          map[string]any     `json:"value"`
	Threshold      *heain.Threshold   `json:"threshold"`
	Confidence     *float64           `json:"confidence"`
	Summary        string             `json:"summary"`
	Factors        []heain.Factor     `json:"factors"`
	Role           string             `json:"role"`
	ModelSHA256    string             `json:"model_sha256"`
	Runtime        string             `json:"runtime"`
	HumanReview    *heain.HumanReview `json:"human_review"`
	Corrects       string             `json:"corrects"`
}

func (d decision) toSDK(defCap string) (heain.Decision, error) {
	if d.Capability == "" {
		d.Capability = defCap
	}
	if d.Capability == "" || d.Decision == "" {
		return heain.Decision{}, errors.New("capability and decision are required")
	}
	return heain.Decision{Capability: d.Capability, Input: d.Input, InputDataClass: d.InputDataClass, InputRef: d.InputRef,
		Decision: d.Decision, Value: d.Value, Threshold: d.Threshold, Confidence: d.Confidence, Summary: d.Summary,
		Factors: d.Factors, Role: d.Role, ModelSHA256: d.ModelSHA256, Runtime: d.Runtime, HumanReview: d.HumanReview, Corrects: d.Corrects}, nil
}

type apiErr struct {
	status int
	code   string
	msg    string
}

func (e *apiErr) Error() string { return e.msg }

func bad(msg string) error { return &apiErr{http.StatusBadRequest, "bad_request", msg} }

// fromSDK maps an SDK error to an answer for the app.
func fromSDK(err error) error {
	var ce *heain.CallError
	if errors.As(err, &ce) {
		return &apiErr{ce.Status, orDefault(ce.Code, "call_failed"), ce.Body}
	}
	var ae *apiErr
	if errors.As(err, &ae) {
		return ae
	}
	return &apiErr{http.StatusBadGateway, "core_error", err.Error()}
}

// LocalHandler is the local API.
func (a *Agent) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, f func(ctx context.Context, r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(a.Token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "the agent token is required (Authorization: Bearer)"}})
				return
			}
			ctx, err := a.ctxFor(r)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "unknown_request", "message": err.Error()}})
				return
			}
			out, err := f(ctx, r)
			if err != nil {
				e := fromSDK(err).(*apiErr)
				writeJSON(w, e.status, map[string]any{"error": map[string]string{"code": e.code, "message": e.msg}})
				return
			}
			writeJSON(w, http.StatusOK, out)
		})
	}
	decode := func(r *http.Request, v any) error {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			return bad("body: " + err.Error())
		}
		return nil
	}

	h("GET /v1/status", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]any{"app": a.App.Manifest.App.ID, "version": a.App.Manifest.App.Version, "instance": a.App.Instance,
			"status": a.App.Status(), "mode": a.App.CurrentMode()}, nil
	})
	h("GET /v1/mode", func(ctx context.Context, r *http.Request) (any, error) {
		m, err := a.App.FetchMode(ctx)
		if err != nil {
			return a.App.CurrentMode(), nil
		}
		return m, nil
	})

	// jobs (this app as a submitter)
	h("POST /v1/jobs", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			Capability     string               `json:"capability"`
			Version        int                  `json:"version"`
			OriginZone     string               `json:"origin_zone"`
			Payload        []byte               `json:"payload_b64"`
			Classification heain.Classification `json:"classification"`
			Requirements   heain.Requirements   `json:"requirements"`
			IdempotencyKey string               `json:"idempotency_key"`
			MaxAttempts    int                  `json:"max_attempts"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		return a.App.Submit(ctx, heain.JobRequest{Capability: q.Capability, Version: q.Version, OriginZone: q.OriginZone, Payload: q.Payload,
			Classification: q.Classification, Requirements: q.Requirements, IdempotencyKey: q.IdempotencyKey, MaxAttempts: q.MaxAttempts})
	})
	h("GET /v1/jobs/{ticket}", func(ctx context.Context, r *http.Request) (any, error) {
		return a.App.Job(ctx, r.PathValue("ticket"))
	})
	h("POST /v1/jobs/{ticket}/wait", func(ctx context.Context, r *http.Request) (any, error) {
		wctx, cancel := context.WithTimeout(ctx, waitFor(r, 60*time.Second))
		defer cancel()
		return a.App.WaitJob(wctx, r.PathValue("ticket"))
	})
	h("POST /v1/jobs/{ticket}/confirm-retrieval", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]bool{"ok": true}, a.App.ConfirmRetrieval(ctx, r.PathValue("ticket"))
	})
	h("POST /v1/jobs/{ticket}/request-erasure", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]bool{"ok": true}, a.App.RequestErasure(ctx, r.PathValue("ticket"))
	})

	// other apps
	h("GET /v1/discover", func(ctx context.Context, r *http.Request) (any, error) {
		v, _ := strconv.Atoi(r.URL.Query().Get("version"))
		inst, err := a.App.Discover(ctx, r.URL.Query().Get("capability"), v)
		return map[string]any{"instances": inst}, err
	})
	h("POST /v1/call", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			App, Capability, Instance, Method, Path string
			Version                                 int
			Body                                    json.RawMessage `json:"body"`
			BodyB64                                 []byte          `json:"body_b64"`
			TimeoutS                                int             `json:"timeout_s"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		var body any
		switch {
		case q.BodyB64 != nil:
			body = q.BodyB64
		case len(q.Body) > 0:
			body = []byte(q.Body)
		}
		var out json.RawMessage
		st, err := a.App.Call(ctx, heain.CallSpec{App: q.App, Capability: q.Capability, Version: q.Version, Instance: q.Instance,
			Method: q.Method, Path: q.Path, Body: body, Out: &out, Timeout: time.Duration(q.TimeoutS) * time.Second})
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			out = json.RawMessage("null")
		}
		return map[string]any{"status": st, "body": out}, nil
	})

	// reasoning records and audit
	h("POST /v1/reason", func(ctx context.Context, r *http.Request) (any, error) {
		var d decision
		if err := decode(r, &d); err != nil {
			return nil, err
		}
		sd, err := d.toSDK("")
		if err != nil {
			return nil, bad(err.Error())
		}
		id, err := a.App.Reason(ctx, sd)
		return map[string]string{"record_id": id}, err
	})
	h("POST /v1/audit", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			Capability string         `json:"capability"`
			Outcome    string         `json:"outcome"`
			Detail     map[string]any `json:"detail"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, a.App.Audit(ctx, q.Capability, q.Outcome, q.Detail)
	})

	// P5 and P7
	h("POST /v1/policy", func(ctx context.Context, r *http.Request) (any, error) {
		var p heain.Proposal
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return a.App.Propose(ctx, p)
	})
	h("GET /v1/policy/{id}", func(ctx context.Context, r *http.Request) (any, error) {
		return a.App.PolicyStatus(ctx, r.PathValue("id"))
	})
	h("POST /v1/broadcast", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			DiscoveryID string         `json:"discovery_id"`
			OriginZone  string         `json:"origin_zone"`
			Payload     map[string]any `json:"payload"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		return a.App.Broadcast(ctx, q.DiscoveryID, q.OriginZone, q.Payload)
	})
	h("GET /v1/broadcasts", func(ctx context.Context, r *http.Request) (any, error) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		bs, next, err := a.App.Broadcasts(ctx, after, limit)
		return map[string]any{"broadcasts": bs, "next": next}, err
	})

	// the offline journal
	h("POST /v1/journal", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			Kind string `json:"kind"`
			Lane string `json:"lane"`
			Data any    `json:"data"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		return a.App.Journal(ctx, q.Kind, q.Lane, q.Data)
	})
	h("GET /v1/journal", func(ctx context.Context, r *http.Request) (any, error) {
		return a.App.JournalProgress(ctx)
	})

	// data at rest: the key never leaves the agent; the app seals and opens
	h("POST /v1/seal", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			Key       string `json:"key"`
			Plaintext []byte `json:"plaintext_b64"`
			AAD       []byte `json:"aad_b64"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		s, err := a.App.Sealer(ctx, q.Key)
		if err != nil {
			return nil, err
		}
		return map[string]any{"sealed_b64": s.Seal(q.Plaintext, q.AAD)}, nil
	})
	h("POST /v1/open", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			Key    string `json:"key"`
			Sealed []byte `json:"sealed_b64"`
			AAD    []byte `json:"aad_b64"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		s, err := a.App.Sealer(ctx, q.Key)
		if err != nil {
			return nil, err
		}
		pt, err := s.Open(q.Sealed, q.AAD)
		if err != nil {
			return nil, &apiErr{http.StatusUnprocessableEntity, "open_failed", err.Error()}
		}
		return map[string]any{"plaintext_b64": pt}, nil
	})
	h("DELETE /v1/keys/{name}", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]bool{"destroyed": true}, a.App.DestroyDataKey(ctx, r.PathValue("name"))
	})

	// models
	h("GET /v1/models/{name}", func(ctx context.Context, r *http.Request) (any, error) {
		return a.App.ResolveModel(ctx, r.PathValue("name"))
	})
	h("POST /v1/models/{name}/fetch", func(ctx context.Context, r *http.Request) (any, error) {
		var q struct {
			Dir string `json:"dir"`
		}
		if err := decode(r, &q); err != nil {
			return nil, err
		}
		if q.Dir == "" {
			return nil, bad("dir is required")
		}
		m, err := a.App.Model(ctx, r.PathValue("name"), q.Dir)
		return map[string]any{"model": m.ModelRef, "path": m.Path}, err
	})
	return mux
}

func waitFor(r *http.Request, d time.Duration) time.Duration {
	if s, err := strconv.Atoi(r.URL.Query().Get("timeout_s")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_, _ = fmt.Fprint(w, "")
	}
}
