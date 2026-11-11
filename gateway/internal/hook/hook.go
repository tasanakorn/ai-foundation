// Package hook defines the pre-request extension point.
//
// A hook sees each request after auth and model routing, before it goes upstream,
// and answers allow, deny, or modify. The transport (NATS today) lives elsewhere;
// this package holds the wire types and the policy that every transport shares.
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"ai-foundation/gateway/internal/keys"
)

const (
	Allow  = "allow"
	Deny   = "deny"
	Modify = "modify"
)

// Request is the JSON sent to the extension.
type Request struct {
	RequestID     string          `json:"request_id"`
	Key           Key             `json:"key"`
	EndUser       string          `json:"end_user"`
	Endpoint      string          `json:"endpoint"`
	Model         string          `json:"model"` // as the client sent it (alias or provider/model)
	Provider      string          `json:"provider"`
	UpstreamModel string          `json:"upstream_model"`
	Stream        bool            `json:"stream"`
	Body          json.RawMessage `json:"body"`                   // the client's request body, unchanged
	BodyOmitted   bool            `json:"body_omitted,omitempty"` // body too big for the transport
}

type Key struct {
	ID       string            `json:"id"`
	TenantID string            `json:"tenant_id"`
	AppID    string            `json:"app_id"`
	Metadata map[string]string `json:"metadata"`
}

// Decision is the JSON the extension replies with.
//
//	{"action":"allow"}
//	{"action":"deny","status":429,"code":"budget_exceeded","message":"daily limit reached"}
//	{"action":"modify","body":{...full replacement request body...}}
type Decision struct {
	Action  string          `json:"action"`
	Status  int             `json:"status,omitempty"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// Hook is one transport (NATS, later HTTP or gRPC).
type Hook interface {
	PreRequest(ctx context.Context, req *Request) (*Decision, error)
}

// ErrUnavailable means the hook could not decide and the policy is fail-closed.
var ErrUnavailable = errors.New("pre-request hook unavailable")

var allowAll = &Decision{Action: Allow}

type Policy struct {
	Timeout  time.Duration
	FailOpen bool
	Models   []string // key allow-list patterns; empty = all
}

// Guard wraps a transport with the shared policy: model filter, timeout, reply checks,
// and fail-open / fail-closed. The returned Hook never returns an invalid Decision.
func Guard(h Hook, p Policy, log *slog.Logger) Hook { return &guard{h, p, log} }

type guard struct {
	next   Hook
	policy Policy
	log    *slog.Logger
}

func (g *guard) PreRequest(ctx context.Context, req *Request) (*Decision, error) {
	if !keys.MatchModel(g.policy.Models, req.Model) {
		return allowAll, nil
	}
	if g.policy.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.policy.Timeout)
		defer cancel()
	}
	d, err := g.next.PreRequest(ctx, req)
	if err == nil {
		err = normalize(d)
	}
	if err != nil {
		g.log.Warn("pre-request hook failed", "request_id", req.RequestID, "fail_open", g.policy.FailOpen, "err", err)
		if g.policy.FailOpen {
			return allowAll, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return d, nil
}

func normalize(d *Decision) error {
	if d == nil {
		return errors.New("empty decision")
	}
	switch d.Action {
	case Allow:
	case Deny:
		if d.Status < 400 || d.Status > 599 {
			d.Status = 403
		}
		if d.Code == "" {
			d.Code = "request_denied"
		}
		if d.Message == "" {
			d.Message = "request denied by policy"
		}
	case Modify:
		var obj map[string]json.RawMessage
		if json.Unmarshal(d.Body, &obj) != nil || obj == nil {
			return errors.New("modify decision needs a JSON object body")
		}
	default:
		return fmt.Errorf("unknown action %q", d.Action)
	}
	return nil
}
