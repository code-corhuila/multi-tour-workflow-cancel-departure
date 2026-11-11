package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/in"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"COMMERCIAL_RESERVATION_BASE_URL": "http://multi-tour-commercial-reservation-api:8083",
		"OPERATIONS_COST_BASE_URL":        "http://multi-tour-operations-cost-api:8084",
		"SERVICE_TOKEN":                   "token-secret-value",
		"JWT_PUBLIC_KEY":                  "-----BEGIN PUBLIC KEY-----\\nabc\\n-----END PUBLIC KEY-----",
		"DATABASE_URL":                    "postgres://app:db-secret-pass@postgres:5432/db",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestU_the_declared_defaults_apply(t *testing.T) {
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.listenAddr != ":8090" || cfg.stepTimeout != 5*time.Second || cfg.stepAttempts != 3 ||
		cfg.jwtIssuer != "multi-tour-security" || cfg.jwtAudience != "multi-tour" {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

// Every refusal names the variable and never its value: the token and the connection string are secrets.
func TestU_config_names_the_bad_variable_and_never_its_value(t *testing.T) {
	cases := map[string]map[string]string{
		"COMMERCIAL_RESERVATION_BASE_URL": {"COMMERCIAL_RESERVATION_BASE_URL": "ftp://x"},
		"OPERATIONS_COST_BASE_URL":        {"OPERATIONS_COST_BASE_URL": ""},
		"SERVICE_TOKEN":                   {"SERVICE_TOKEN": ""},
		"DATABASE_URL":                    {"DATABASE_URL": ""},
		"JWT_PUBLIC_KEY":                  {"JWT_PUBLIC_KEY": ""},
		"STEP_TIMEOUT":                    {"STEP_TIMEOUT": "99s"},
		"STEP_ATTEMPTS":                   {"STEP_ATTEMPTS": "9"},
	}
	for want, change := range cases {
		_, err := loadConfig(env(change))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: error %v, want it to name the variable", want, err)
		}
		if strings.Contains(err.Error(), "token-secret-value") || strings.Contains(err.Error(), "db-secret-pass") {
			t.Fatalf("%s: the error repeats a secret: %v", want, err)
		}
	}
}

// The check of the issuer and of the audience would be silently off with an empty one (jwt v5 ignores an empty expectation).
func TestU_an_issuer_or_an_audience_that_is_not_set_takes_the_platforms_and_is_never_empty(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"JWT_ISSUER": "other-issuer", "JWT_AUDIENCE": "other-audience"}))
	if err != nil || cfg.jwtIssuer != "other-issuer" || cfg.jwtAudience != "other-audience" {
		t.Fatalf("err %v, config %+v", err, cfg)
	}
	cfg, _ = loadConfig(env(map[string]string{"JWT_ISSUER": "", "JWT_AUDIENCE": ""}))
	if cfg.jwtIssuer == "" || cfg.jwtAudience == "" {
		t.Fatalf("an empty issuer or audience reached the verifier: %+v", cfg)
	}
}

func publicKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// The key may come inline (an environment file holds it on one line with the line breaks written as backslash-n) or as a file; the
// inline one wins; a key that cannot be read stops the start naming the variable.
func TestC_the_public_key_is_read_inline_or_from_a_file_and_a_bad_one_stops_the_start(t *testing.T) {
	good := publicKeyPEM(t)
	if _, err := readPublicKey(config{publicKey: strings.ReplaceAll(good, "\n", `\n`)}); err != nil {
		t.Fatalf("inline: %v", err)
	}
	file := filepath.Join(t.TempDir(), "public.pem")
	if err := os.WriteFile(file, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublicKey(config{publicKeyFile: file}); err != nil {
		t.Fatalf("file: %v", err)
	}
	if _, err := readPublicKey(config{publicKey: strings.ReplaceAll(good, "\n", `\n`), publicKeyFile: filepath.Join(t.TempDir(), "missing")}); err != nil {
		t.Fatalf("the inline key must win over a file that is not there: %v", err)
	}
	if _, err := readPublicKey(config{publicKey: "not a key"}); err == nil || !strings.Contains(err.Error(), "JWT_PUBLIC_KEY") {
		t.Fatalf("bad inline key: %v", err)
	}
	if _, err := readPublicKey(config{publicKeyFile: filepath.Join(t.TempDir(), "missing")}); err == nil || !strings.Contains(err.Error(), "JWT_PUBLIC_KEY_FILE") {
		t.Fatalf("missing file: %v", err)
	}
}

// A saga here has one call for each reservation, so no arithmetic bound covers it: the request that starts it gets a budget shorter than the 90 s
// the gateway allows, and what is not done when it ends goes on in the resumption.
func TestU_the_request_that_starts_a_saga_has_a_budget_below_the_gateway_and_the_server_outlasts_it(t *testing.T) {
	if startBudget <= 0 || startBudget >= 90*time.Second {
		t.Fatalf("budget %v: it must be positive and shorter than the 90 s of the gateway", startBudget)
	}
	srv := newServer(config{listenAddr: ":8090"}, http.NewServeMux())
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.IdleTimeout == 0 || srv.MaxHeaderBytes == 0 || srv.WriteTimeout <= startBudget {
		t.Fatalf("limits %+v, budget %v", srv, startBudget)
	}
	if shutdownGrace() <= startBudget {
		t.Fatal("a stop must wait longer than a started saga can run")
	}
}

// A saga left running is taken again only after it has not moved for longer than its start budget, so a live one is never taken.
func TestU_a_saga_is_stale_for_longer_than_the_budget_of_its_start(t *testing.T) {
	if staleAfter <= startBudget {
		t.Fatalf("stale after %v, the start budget is %v", staleAfter, startBudget)
	}
}

type fakeUseCase struct {
	startCtx context.Context
	getCtx   context.Context
}

func (f *fakeUseCase) Start(ctx context.Context, _ in.StartCommand) (saga.Saga, bool, error) {
	f.startCtx = ctx
	return saga.Saga{}, true, nil
}

func (f *fakeUseCase) Get(ctx context.Context, _, _ string) (saga.Saga, error) {
	f.getCtx = ctx
	return saga.Saga{}, nil
}

// The use case that the entry sees gives the start a deadline and leaves the read as it is; what the start does not finish in its budget is
// RUNNING and goes on by itself.
func TestC_the_start_gets_a_deadline_and_the_read_does_not(t *testing.T) {
	inner := &fakeUseCase{}
	uc := budgeted{inner}
	if _, created, err := uc.Start(context.Background(), in.StartCommand{}); err != nil || !created {
		t.Fatalf("created %v, err %v", created, err)
	}
	deadline, ok := inner.startCtx.Deadline()
	if !ok || time.Until(deadline) > startBudget || time.Until(deadline) < startBudget-5*time.Second {
		t.Fatalf("deadline %v, ok %v: want about %v from now", deadline, ok, startBudget)
	}
	if _, err := uc.Get(context.Background(), "t", "i"); err != nil {
		t.Fatal(err)
	}
	if _, ok := inner.getCtx.Deadline(); ok {
		t.Fatal("a read has no budget of its own")
	}
}
