// Package participants calls the internal routes of commercial-reservation and operations-cost for the saga. It is the only place that
// knows their paths, their bodies and how their answers are told apart.
package participants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/correlation"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	maxReplyBytes    = 1 << 20
	maxBackoff       = 30 * time.Second
	defaultTimeout   = 5 * time.Second
	defaultAttempts  = 3
	defaultBaseDelay = 200 * time.Millisecond
)

// Config bounds every participant call (ADR-004 D10).
type Config struct {
	Token     string        // SERVICE_TOKEN of this saga, never the person's token (norm 5.8.2)
	Timeout   time.Duration // per attempt
	Attempts  int           // total attempts, at least 1
	BaseDelay time.Duration // ceiling of the first wait; it doubles on each retry
}

type client struct {
	baseURL string
	cfg     Config
	http    *http.Client
}

func newClient(baseURL string, cfg Config) *client {
	// a configuration left at zero gets what the plan says (ADR-004 D10: three attempts, a base wait of 200 ms) and not no retry at all; a
	// composition root that sets the attempts is trusted with the wait, which tests set to zero
	if cfg.Attempts <= 0 {
		cfg.Attempts = defaultAttempts
		cfg.BaseDelay = max(cfg.BaseDelay, defaultBaseDelay)
	}
	cfg.BaseDelay = max(cfg.BaseDelay, 0)
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	// A redirect would take the credential elsewhere, and what it did there is unknown: it is not followed.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{baseURL: strings.TrimRight(baseURL, "/"), cfg: cfg, http: &http.Client{CheckRedirect: noRedirect}}
}

// departurePath is the path of a departure of a tenant on an internal route; the code must be plain.
func departurePath(prefix, tenantID, code, rest string) (string, error) {
	if !saga.ValidDepartureCode(code) || !saga.ValidID(tenantID) {
		return "", fmt.Errorf("the tenant or the code of the departure is not plain: %w", out.ErrRefused)
	}
	return prefix + "/internal/tenants/" + url.PathEscape(tenantID) + "/departures/" + url.PathEscape(code) + rest, nil
}

// idPath is the path of a route of a tenant that names the departure by its id, as commercial-reservation does: a code would be a 404 there.
func idPath(prefix, tenantID, departureID, rest string) (string, error) {
	if !saga.ValidID(departureID) || !saga.ValidID(tenantID) {
		return "", fmt.Errorf("the tenant or the departure is not an id: %w", out.ErrRefused)
	}
	return prefix + "/internal/tenants/" + url.PathEscape(tenantID) + "/departures/" + url.PathEscape(departureID) + rest, nil
}

// errUnclear is an answer that says nothing about whether the work was done (network, 429, 5xx, a redirect, an answer without the
// fields that were promised). It is none of the sentinels of the port, and it never carries the text of the participant.
func errUnclear(detail string) error {
	return errors.New("the participant gave no clear answer: " + detail)
}

// do sends the request and returns the status and body of a clear answer: a 2xx or a 4xx other than 429, 408 and 425. Network failures,
// those three and 5xx are repeated with full jitter and the same key; what is left after the last attempt is errUnclear.
func (c *client) do(ctx context.Context, method, path, key string, body any) (int, []byte, error) {
	var payload []byte // a read has no body
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, nil, fmt.Errorf("encode the request: %w", err)
		}
	}
	corr := correlation.From(ctx)
	if !correlation.Valid(corr) {
		corr = correlation.NewID()
	}
	last := errUnclear("no answer")
	for attempt := 1; attempt <= c.cfg.Attempts; attempt++ {
		if attempt > 1 {
			ceiling := min(c.cfg.BaseDelay<<min(attempt-2, 10), maxBackoff)
			if sleepFor(ctx, rand.N(ceiling+1)) != nil {
				break
			}
		}
		status, reply, err := c.once(ctx, method, path, key, corr, payload)
		switch {
		case err != nil:
			last = errUnclear("no answer")
		case status >= 200 && status < 300, status >= 400 && status < 500 && !transient(status) && !credentialFault(status):
			return status, reply, nil
		default:
			last = errUnclear("status " + strconv.Itoa(status))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return 0, nil, last
}

// credentialFault is a 401 or 403: the token of the saga was refused. That is a fault of configuration and says nothing about the work, so
// it is not a clear answer; the saga stays running and is asked again once the token is fixed.
func credentialFault(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func transient(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status == http.StatusTooEarly
}

func (c *client) once(ctx context.Context, method, path, key, corr string, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("X-Correlation-Id", corr)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	return resp.StatusCode, reply, err
}

func sleepFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// refusal turns a clear 4xx into a sentinel of the port: a 404 is not found, any other is a refusal of the owner's rules. It says which status
// it was, so that a 400 of ours does not look like a rule, and the text of the answer is dropped.
func refusal(status int) error {
	if status == http.StatusNotFound {
		return fmt.Errorf("status %d: %w", status, out.ErrNotFound)
	}
	return fmt.Errorf("status %d: %w", status, out.ErrRefused)
}

// notID is a tenant, a departure or a reservation that is not an id: it goes into a path or a body, so it is refused before any call.
func notID() error {
	return fmt.Errorf("the tenant, the departure or the reservation is not an id: %w", out.ErrRefused)
}

// equalIDs compares two ids as the text of a UUID, which a service may write in either case.
func equalIDs(a, b string) bool { return strings.EqualFold(a, b) }
