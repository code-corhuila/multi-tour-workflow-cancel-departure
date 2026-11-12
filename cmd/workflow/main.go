package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/in/httpapi"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/out/participants"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/out/state/postgres"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/usecase"
)

const (
	retryBaseDelay = 200 * time.Millisecond
	resumeEvery    = time.Minute
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("workflow stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	key, err := readPublicKey(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Open(ctx, cfg.databaseURL, postgres.Limits{MaxConns: 10, ConnectTimeout: 5 * time.Second, StatementTimeout: 5 * time.Second})
	if err != nil {
		return err
	}
	defer pool.Close()

	participantCfg := participants.Config{Token: cfg.serviceToken, Timeout: cfg.stepTimeout, Attempts: cfg.stepAttempts, BaseDelay: retryBaseDelay}
	cancel := usecase.NewCancelDeparture(participants.NewOperations(cfg.operationsURL, participantCfg),
		participants.NewReservations(cfg.reservationsURL, participantCfg), postgres.NewStore(pool), time.Now, log)

	srv := newServer(cfg, httpapi.New(budgeted{cancel}, httpapi.NewVerifier(key, cfg.jwtIssuer, cfg.jwtAudience), log, cfg.starters))
	serving := make(chan error, 1)
	go func() { serving <- srv.ListenAndServe() }()
	log.Info("workflow listening", "addr", cfg.listenAddr)

	// A saga left RUNNING because a participant gave no clear answer, or because its start ran out of its budget, is taken again every minute
	// once it has not moved for longer than that budget (every save moves it, so a live saga is never taken; two instances never take the same
	// one because taking it is a save). It runs after the listener is up, so the health check answers while the participants are reached.
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		for {
			resumeCtx, stopPass := context.WithTimeout(ctx, resumePass)
			if _, err := cancel.Resume(resumeCtx, time.Now().Add(-staleAfter)); err != nil {
				log.Error("resuming the sagas left running failed", "error", err.Error())
			}
			stopPass()
			select {
			case <-ctx.Done():
				return
			case <-time.After(resumeEvery):
			}
		}
	}()

	select {
	case err := <-serving:
		return err
	case <-ctx.Done():
	}
	// stop accepting, let the running requests answer, then close the pool (deferred)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace())
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-resumed
	log.Info("workflow stopped")
	return nil
}

// readPublicKey reads the key of the security service: inline (an environment file holds it on one line with the line breaks written as
// backslash-n) or from a file; the inline one wins. A key that cannot be read stops the start and names the variable.
func readPublicKey(cfg config) (*rsa.PublicKey, error) {
	variable, pem := "JWT_PUBLIC_KEY", []byte(strings.ReplaceAll(cfg.publicKey, `\n`, "\n"))
	if cfg.publicKey == "" {
		variable = "JWT_PUBLIC_KEY_FILE"
		data, err := os.ReadFile(cfg.publicKeyFile)
		if err != nil {
			return nil, errors.New("JWT_PUBLIC_KEY_FILE cannot be read")
		}
		pem = data
	}
	key, err := httpapi.ParsePublicKey(pem)
	if err != nil {
		return nil, errors.New(variable + ": " + err.Error())
	}
	return key, nil
}
