package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"ai-foundation/gateway/internal/hook"
)

// PreRequestHook is the NATS transport for hook.Hook: one request/reply per call.
// Extensions subscribe to the subject with a queue group, so several copies share the load.
// Wrap it with hook.Guard for timeout, model filter and fail-open handling.
func (b *Bus) PreRequestHook(subject string) hook.Hook { return natsHook{b, subject} }

type natsHook struct {
	b       *Bus
	subject string
}

func (h natsHook) PreRequest(ctx context.Context, req *hook.Request) (*hook.Decision, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// NATS messages are small (1 MiB by default). Big requests (images, long documents)
	// go without the body; the extension can still allow or deny, but not modify.
	omitted := false
	if int64(len(data)) > h.b.nc.MaxPayload() {
		omitted = true
		slim := *req
		slim.Body, slim.BodyOmitted = nil, true
		if data, err = json.Marshal(&slim); err != nil {
			return nil, err
		}
	}
	ctx, cancel := withDefaultTimeout(ctx, time.Second)
	defer cancel()
	msg, err := h.b.nc.RequestWithContext(ctx, h.subject, data)
	if err != nil {
		return nil, err
	}
	var d hook.Decision
	if err := json.Unmarshal(msg.Data, &d); err != nil {
		return nil, fmt.Errorf("bad hook reply: %w", err)
	}
	if omitted && d.Action == hook.Modify {
		return nil, fmt.Errorf("hook tried to modify a request whose body it did not receive")
	}
	return &d, nil
}
