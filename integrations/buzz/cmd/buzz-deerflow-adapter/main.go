package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/omengye/deerflow-acp/integrations/buzz/internal/adapter"
)

var version = "0.3.0-dev"

func run(args []string) error {
	flags := flag.NewFlagSet("buzz-deerflow-adapter", flag.ContinueOnError)
	configPath := flags.String("config", "adapter.toml", "adapter TOML path")
	envFile := flags.String("env-file", "", "environment file (default: .env beside adapter TOML)")
	check := flags.Bool("check", false, "validate identity and local executables; no relay, ACP or database changes")
	once := flags.Bool("once", false, "poll and process once (may execute tasks and send replies)")
	showVersion := flags.Bool("version", false, "print adapter version")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *showVersion {
		fmt.Println("buzz-deerflow-adapter", version)
		return nil
	}
	if *envFile == "" {
		*envFile = filepath.Join(filepath.Dir(*configPath), ".env")
	}
	if err := adapter.LoadEnv(*envFile); err != nil {
		return err
	}
	c, err := adapter.LoadConfig(*configPath)
	if err != nil {
		return errors.New(c.Redact(err.Error()))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = c.CheckDeployment(ctx); err != nil {
		return errors.New(c.Redact(err.Error()))
	}
	if *check {
		fmt.Println("OK: Buzz identity, sender policy and native Go ACP v2 deployment validated.")
		fmt.Println("No relay connection, ACP startup or state database changes were performed.")
		return nil
	}
	level := slog.LevelInfo
	switch strings.ToUpper(c.Adapter.LogLevel) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	app, err := adapter.NewApp(c)
	if err != nil {
		return errors.New(c.Redact(err.Error()))
	}
	defer app.Close()
	slog.Info("Starting native Buzz adapter", "version", version, "permission_policy", c.Deerflow.PermissionPolicy, "acknowledgement_enabled", c.Buzz.Acknowledgement.Enabled, "acknowledgement_emoji", c.Buzz.Acknowledgement.Emoji)
	if err = app.Run(ctx, *once); err != nil && !errors.Is(err, context.Canceled) {
		return errors.New(c.Redact(err.Error()))
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "buzz-deerflow-adapter:", err)
		os.Exit(1)
	}
}
