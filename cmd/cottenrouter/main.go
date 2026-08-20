package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/TaJirax/CottenRouter/internal/catalog"
	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/installer"
	"github.com/TaJirax/CottenRouter/internal/router"
	"github.com/TaJirax/CottenRouter/internal/tui"
)

func main() {
	if len(os.Args) < 2 {
		if err := runTUI(nil); err != nil {
			fmt.Fprintln(os.Stderr, "cottenrouter:", err)
			os.Exit(1)
		}
		return
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
	case "tui", "dashboard":
		err = runTUI(os.Args[2:])
	case "install":
		err = installProject(os.Args[2:])
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
	if len(cfg.TLSListeners) > 0 {
		fmt.Printf(", %d TLS/SNI listener(s)", len(cfg.TLSListeners))
	}
	fmt.Printf(", %d UDP workers, %d queued packets max", cfg.Limits.UDPWorkers, cfg.Limits.UDPQueue)
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

func runTUI(args []string) error {
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	configPath := flags.String("config", "/etc/cottenrouter/config.json", "router configuration path")
	admin := flags.String("admin", "127.0.0.1:9088", "local CottenRouter admin address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if cfg, err := config.Load(*configPath); err == nil {
		*admin = cfg.AdminListen
	}
	return tui.Run(*admin, *configPath)
}

func installProject(args []string) error {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	project := flags.String("project", "", "project ID from the live catalog")
	domain := flags.String("domain", "", "primary delegated DNS domain")
	port := flags.String("port", "0", "private loopback DNS port")
	extra := flags.String("extra-domains", "", "thefeed extra domains")
	chat := flags.String("chat-domains", "", "thefeed chat domains")
	tcpEnabled := flags.Bool("tcp", false, "enable backend DNS-over-TCP")
	dotEnabled := flags.Bool("dot", false, "enable CottenDNS DoT")
	dohEnabled := flags.Bool("doh", false, "enable CottenDNS DoH")
	dotDomain := flags.String("dot-domain", "", "DoT SNI hostname")
	dohDomain := flags.String("doh-domain", "", "DoH SNI hostname")
	routerConfig := flags.String("router-config", "/etc/cottenrouter/config.json", "router configuration path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	privatePort, err := strconv.Atoi(*port)
	if err != nil {
		return fmt.Errorf("invalid --port: %w", err)
	}
	request := installer.Request{ProjectID: *project, Domain: *domain, PrivatePort: privatePort, ExtraDomains: *extra, ChatDomains: *chat, EnableTCP: *tcpEnabled, EnableDoT: *dotEnabled, EnableDoH: *dohEnabled, DoTDomain: *dotDomain, DoHDomain: *dohDomain, RouterConfig: *routerConfig}
	plan, err := installer.DefaultManager().Install(context.Background(), request, func(message string) { fmt.Println("  •", message) })
	if err != nil {
		return err
	}
	for _, note := range plan.Notes {
		fmt.Println("  !", note)
	}
	fmt.Println("Installation completed safely.")
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: cottenrouter <tui|serve|install|check|catalog|slipgate-import> [options]")
}
