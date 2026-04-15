package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oss-sync/config"
	"github.com/oss-sync/db"
	"github.com/oss-sync/syncer"
	"github.com/oss-sync/tui"

	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var (
		cfgPath string
		mode    string
	)

	root := &cobra.Command{
		Use:   "oss-sync",
		Short: "Sync objects from Alibaba Cloud OSS to Huawei Cloud OBS (or any S3-compatible store)",
	}

	// ── sync command ────────────────────────────────────────────────────
	var noTUI bool

	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Start the sync process",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			if mode != "" {
				cfg.Sync.Mode = mode
			}

			s, err := syncer.New(cfg)
			if err != nil {
				return fmt.Errorf("init syncer: %w", err)
			}
			defer s.Close()

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			useTUI := !noTUI && tui.IsTTY()
			if !useTUI {
				// Plain mode: run sync in foreground with log output.
				return s.RunWithContext(ctx)
			}

			logWriter := log.Writer()
			log.SetOutput(io.Discard)
			defer log.SetOutput(logWriter)

			// TUI mode: run sync in a background goroutine while the TUI
			// owns the terminal. Open the DB a second time (read-only poll).
			doneCh := make(chan error, 1)
			syncDone := make(chan struct{})
			go func() {
				doneCh <- s.RunWithContext(ctx)
				close(syncDone)
			}()

			database, err := db.Open(cfg.Sync.DBPath)
			if err != nil {
				return fmt.Errorf("open stats db: %w", err)
			}
			defer database.Close()

			userQuit, tuiErr := tui.RunSyncTUI(database, cfg.Scopes(), 500*time.Millisecond, syncDone)
			if userQuit {
				cancel()
			}

			// Wait for sync to finish.
			syncErr := <-doneCh
			if tuiErr != nil {
				return tuiErr
			}
			if userQuit && errors.Is(syncErr, context.Canceled) {
				return nil
			}
			return syncErr
		},
	}

	syncCmd.Flags().StringVarP(&cfgPath, "config", "c", "config.yaml", "path to config file")
	syncCmd.Flags().StringVarP(&mode, "mode", "m", "", "sync mode: full | incremental (overrides config)")
	syncCmd.Flags().BoolVar(&noTUI, "no-tui", false, "disable TUI and print plain log output")

	// ── stats command ───────────────────────────────────────────────────
	var (
		statsInterval time.Duration
		statsNoTUI    bool
		statsWatch    bool
	)

	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Show sync statistics from the database",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			database, err := db.Open(cfg.Sync.DBPath)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer database.Close()

			useTUI := (statsWatch || tui.IsTTY()) && !statsNoTUI
			if useTUI {
				_, err := tui.RunTUI(database, cfg.Scopes(), statsInterval)
				return err
			}
			return tui.PrintHeadless(os.Stdout, database, cfg.Scopes())
		},
	}
	statsCmd.Flags().StringVarP(&cfgPath, "config", "c", "config.yaml", "path to config file")
	statsCmd.Flags().DurationVar(&statsInterval, "interval", 500*time.Millisecond, "TUI refresh interval")
	statsCmd.Flags().BoolVarP(&statsWatch, "watch", "w", false, "force TUI watch mode even without a TTY")
	statsCmd.Flags().BoolVar(&statsNoTUI, "no-tui", false, "disable TUI and print a plain snapshot")

	var failedLimit int
	failedCmd := &cobra.Command{
		Use:   "failed",
		Short: "Show failed files and error reasons from the database",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			database, err := db.Open(cfg.Sync.DBPath)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer database.Close()

			failed, err := database.FailedRecordsForScopes(cfg.Scopes(), failedLimit)
			if err != nil {
				return err
			}
			if len(failed) == 0 {
				fmt.Fprintln(os.Stdout, "No failed files.")
				return nil
			}

			for i, record := range failed {
				fmt.Fprintf(os.Stdout, "%d. %s -> %s\n", i+1, record.SourceKey, record.Key)
				fmt.Fprintf(os.Stdout, "   size: %d bytes\n", record.Size)
				fmt.Fprintf(os.Stdout, "   error: %s\n", record.ErrorMsg)
			}
			return nil
		},
	}
	failedCmd.Flags().StringVarP(&cfgPath, "config", "c", "config.yaml", "path to config file")
	failedCmd.Flags().IntVar(&failedLimit, "limit", 100, "maximum failed files to show")

	root.AddCommand(syncCmd, statsCmd, failedCmd)
	return root
}
