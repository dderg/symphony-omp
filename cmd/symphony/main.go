package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	symphony "symphony-omp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(stderr, "Usage: symphony [path-to-WORKFLOW.md]")
		return 0
	}
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		logger.Error("invalid command arguments", "usage", "symphony [path-to-WORKFLOW.md]")
		return 1
	}
	path := "WORKFLOW.md"
	if len(args) == 1 {
		path = args[0]
	}
	orchestrator, err := symphony.NewOrchestrator(ctx, path, logger)
	if err != nil {
		logger.Error("startup failed", "workflow", path, "error", err)
		return 1
	}
	logger.Info("service started", "workflow", path)
	if err = orchestrator.Run(ctx); err != nil {
		logger.Error("service failed", "error", err)
		return 1
	}
	logger.Info("service stopped", "outcome", "completed")
	return 0
}
