package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/in"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	// the gateway sends the sagas to the port 8090 of each workflow
	defaultListenAddr   = ":8090"
	defaultStepTimeout  = 5 * time.Second
	defaultStepAttempts = 3
	maxStepTimeout      = 30 * time.Second
	maxStepAttempts     = 5
	// A saga here makes one call for each reservation of the departure, so no arithmetic bound covers it. The request that starts it gets this
	// budget, shorter than the 90 s the gateway allows; what is not done when it ends is saved as RUNNING and goes on in the resumption.
	startBudget = 60 * time.Second
	// a saga left RUNNING is taken again after this long without moving, longer than the budget of its start: every save moves it, so a live
	// saga is never taken, and two instances never take the same one because taking it is a save
	staleAfter = 2 * time.Minute
	// one pass of the resumption over the sagas left running
	resumePass = 5 * time.Minute

	defaultIssuer   = "multi-tour-security"
	defaultAudience = "multi-tour"
)

type config struct {
	listenAddr             string
	reservationsURL        string
	operationsURL          string
	serviceToken           string
	publicKeyFile          string
	publicKey              string
	jwtIssuer, jwtAudience string
	databaseURL            string
	stepTimeout            time.Duration
	stepAttempts           int
}

// loadConfig reads the variables of .env.example. Every refusal names the variable and never its value: the token and the connection
// string are secrets. The issuer and the audience are never empty: the library skips the check when they are.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		listenAddr:      valueOr(getenv("LISTEN_ADDR"), defaultListenAddr),
		reservationsURL: getenv("COMMERCIAL_RESERVATION_BASE_URL"),
		operationsURL:   getenv("OPERATIONS_COST_BASE_URL"),
		serviceToken:    getenv("SERVICE_TOKEN"),
		publicKeyFile:   getenv("JWT_PUBLIC_KEY_FILE"),
		publicKey:       getenv("JWT_PUBLIC_KEY"),
		jwtIssuer:       valueOr(getenv("JWT_ISSUER"), defaultIssuer),
		jwtAudience:     valueOr(getenv("JWT_AUDIENCE"), defaultAudience),
		databaseURL:     getenv("DATABASE_URL"),
		stepTimeout:     defaultStepTimeout,
		stepAttempts:    defaultStepAttempts,
	}
	var problems []error
	refuse := func(name, why string) { problems = append(problems, fmt.Errorf("%s %s", name, why)) }

	for _, check := range []struct{ name, value string }{{"COMMERCIAL_RESERVATION_BASE_URL", cfg.reservationsURL}, {"OPERATIONS_COST_BASE_URL", cfg.operationsURL}} {
		if u, err := url.Parse(check.value); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			refuse(check.name, "must be an http or https URL")
		}
	}
	if cfg.serviceToken == "" {
		refuse("SERVICE_TOKEN", "is required")
	}
	if cfg.databaseURL == "" {
		refuse("DATABASE_URL", "is required")
	}
	if cfg.publicKeyFile == "" && cfg.publicKey == "" {
		refuse("JWT_PUBLIC_KEY", "or JWT_PUBLIC_KEY_FILE is required")
	}
	if raw := getenv("STEP_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < time.Second || d > maxStepTimeout {
			refuse("STEP_TIMEOUT", "must be a duration from 1s to 30s")
		} else {
			cfg.stepTimeout = d
		}
	}
	if raw := getenv("STEP_ATTEMPTS"); raw != "" {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 1 || n > maxStepAttempts {
			refuse("STEP_ATTEMPTS", "must be a number from 1 to 5")
		} else {
			cfg.stepAttempts = n
		}
	}
	return cfg, errors.Join(problems...)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// shutdownGrace is how long a stop waits for the requests still running: the budget of a start plus a little, so the pool is not closed under a
// saga that is half way through a step. The container stop period must be longer.
func shutdownGrace() time.Duration { return startBudget + 5*time.Second }

// newServer declares every limit (Annex C). A saga runs inside the request that starts it, up to its budget, so the write timeout is the budget
// plus a little: a shorter one would cut the answer of a saga that is still correct.
func newServer(cfg config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      startBudget + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// budgeted gives the start of a saga the budget of its request, and leaves the read as it is.
type budgeted struct{ in.CancelDeparture }

func (b budgeted) Start(ctx context.Context, cmd in.StartCommand) (saga.Saga, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, startBudget)
	defer cancel()
	return b.CancelDeparture.Start(ctx, cmd)
}
