// Command api runs the public Go orchestration service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"book-text-editor/api"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	app, err := api.NewServerFromEnv(logger)
	if err != nil {
		return err
	}
	defer app.Close()

	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	shutdownContext, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("API listening", "address", listener.Addr().String())
		serverErrors <- server.Serve(listener)
	}()

	select {
	case <-shutdownContext.Done():
		contextWithTimeout, cancel := context.WithTimeout(
			context.Background(),
			20*time.Second,
		)
		defer cancel()
		if err := server.Shutdown(contextWithTimeout); err != nil {
			return err
		}
		err := <-serverErrors
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil

	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
