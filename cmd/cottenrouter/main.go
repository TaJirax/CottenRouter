package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/TaJirax/CottenRouter/internal/catalog"
	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/router"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "check":
		err = check(os.Args[2:])
	case "catalog":
		err = printCatalog(os.Args[2:])
	case "slipgate-import":
		err = importSlipGate(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cottenrouter:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "cottenrouter.json", "path to JSON configuration")
	debug := flags.Bool("debug", false, "enable debug logs")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	server, err := router.New(cfg, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.ListenAndServe(ctx)
}

func check(args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	configPath := flags.String("config", "cottenrouter.json", "path to JSON configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	fmt.Printf("configuration OK: %d routes, UDP %s", len(cfg.Routes), cfg.ListenUDP)
	if cfg.ListenTCP != "" {
		fmt.Printf(", TCP %s", cfg.ListenTCP)
	}
	fmt.Println()
	return nil
}

func printCatalog(args []string) error {
	flags := flag.NewFlagSet("catalog", flag.ContinueOnError)
	offline := flags.Bool("offline", false, "use bundled fallback metadata without checking GitHub")
	if err := flags.Parse(args); err != nil {
		return err
	}
	projects := catalog.Projects()
	if !*offline {
		var err error
		projects, err = catalog.DefaultResolver().Latest(context.Background())
		if err != nil {
			return fmt.Errorf("refresh installer catalog (use --offline only for inspection): %w", err)
		}
	}
	for _, project := range projects {
		fmt.Printf("%s (%s)\n  repo: %s\n  branch checked: %s\n  installer: %s\n  upstream command: %s\n  service: %s\n",
			project.Name, project.ID, project.Repository, project.DefaultBranch, project.InstallerURL, project.InstallCommand,
			project.Service)
		if project.Routable {
			fmt.Printf("  router setting: %s -> %s\n\n", project.ListenSetting, project.DefaultBackend)
		} else {
			fmt.Printf("  integration: %s\n\n", project.ListenSetting)
		}
	}
	return nil
}

func importSlipGate(args []string) error {
	flags := flag.NewFlagSet("slipgate-import", flag.ContinueOnError)
	input := flags.String("input", "/etc/slipgate/config.json", "path to SlipGate config.json")
	output := flags.String("output", "-", "output path, or - for stdout")
	force := flags.Bool("force", false, "replace an existing output file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.NewFromSlipGate(*input)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *output == "-" {
		_, err = os.Stdout.Write(data)
		return err
	}
	mode := os.O_CREATE | os.O_WRONLY
	if *force {
		mode |= os.O_TRUNC
	} else {
		mode |= os.O_EXCL
	}
	file, err := os.OpenFile(*output, mode, 0644)
	if err != nil {
		return fmt.Errorf("create output config: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: cottenrouter <serve|check|catalog|slipgate-import> [options]")
}
