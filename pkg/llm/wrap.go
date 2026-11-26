package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"
)

// WithRetry retries retryable failures with exponential backoff and jitter,
// honouring Retry-After when the provider sends it.
//
// A streaming call is retried only if it failed before any event reached
// the handler; otherwise the caller would see duplicated output.
func WithRetry(c Client, retries int, logger *slog.Logger) Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &retrying{Client: c, retries: retries, logger: logger}
}

// retryBase is the first backoff delay; tests shorten it.
var retryBase = time.Second

type retrying struct {
	Client
	retries int
	logger  *slog.Logger
}

func (r *retrying) Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%s: invalid request: %w", r.Name(), err)
	}
	var lastErr error
	for attempt := 0; attempt <= r.retries; attempt++ {
		h, emitted := trackEmitted(handler)
		resp, err := r.Client.Chat(ctx, req, h)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil || !IsRetryable(err) || attempt == r.retries || *emitted {
			break
		}
		wait := time.Duration(1<<attempt) * retryBase
		wait += time.Duration(rand.Int64N(int64(wait / 2)))
		var ae *APIError
		if errors.As(err, &ae) && ae.RetryAfter > 0 {
			wait = min(ae.RetryAfter, 60*time.Second)
		}
		r.logger.Warn("llm request failed; retrying", "model", r.Name(), "attempt", attempt+1, "wait", wait, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

// trackEmitted wraps a handler and reports whether it was called.
func trackEmitted(h EventHandler) (EventHandler, *bool) {
	emitted := new(bool)
	if h == nil {
		return nil, emitted
	}
	return func(e Event) {
		*emitted = true
		h(e)
	}, emitted
}

type fallback struct {
	clients []Client
	logger  *slog.Logger
}

func (f *fallback) Name() string {
	names := make([]string, len(f.clients))
	for i, c := range f.clients {
		names[i] = c.Name()
	}
	return strings.Join(names, " -> ")
}

func (f *fallback) Chat(ctx context.Context, req *Request, handler EventHandler) (*Response, error) {
	var errs []error
	for i, c := range f.clients {
		h, emitted := trackEmitted(handler)
		resp, err := c.Chat(ctx, req, h)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %w", c.Name(), err))
		if *emitted {
			// Part of a reply was already shown; a different model would
			// start over and the output would not make sense.
			break
		}
		if i < len(f.clients)-1 {
			f.logger.Warn("llm model failed; falling back", "from", c.Name(), "to", f.clients[i+1].Name(), "error", err)
		}
	}
	return nil, errors.Join(errs...)
}
