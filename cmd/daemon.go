package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/Kwutzke/holepunch/internal/config"
	"github.com/Kwutzke/holepunch/internal/daemon"
	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/ip"
	"github.com/Kwutzke/holepunch/internal/proxy"
	"github.com/Kwutzke/holepunch/internal/resolver"
	"github.com/Kwutzke/holepunch/internal/session"
)

const defaultResolverAddr = "127.0.0.1:15353"

var configPath string

var daemonCmd = &cobra.Command{
	Use:    "daemon",
	Short:  "Start the daemon process (internal)",
	Hidden: true,
	RunE:   runDaemon,
}

func init() {
	daemonCmd.Flags().StringVar(&configPath, "config", config.DefaultConfigPath(), "path to config file")
	rootCmd.AddCommand(daemonCmd)
}

func runDaemon(_ *cobra.Command, _ []string) error {
	pidPath := daemon.DefaultPIDPath()

	if err := daemon.WritePIDFile(pidPath); err != nil {
		return fmt.Errorf("acquiring PID lock: %w", err)
	}
	defer daemon.Cleanup(socketPath, pidPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// Start embedded DNS resolver.
	dnsResolver := resolver.New(defaultResolverAddr)
	if err := dnsResolver.Start(); err != nil {
		return fmt.Errorf("starting DNS resolver: %w", err)
	}
	defer dnsResolver.Stop()
	log.Printf("DNS resolver listening on %s", defaultResolverAddr)

	dnsMgr := resolver.NewDNSAdapter(dnsResolver)
	sessMgr := session.NewSSMManager(nil)
	ipAlloc := ip.New()

	eng := engine.New(cfg, dnsMgr, sessMgr, ipAlloc, realProxyFactory{})

	srv, err := daemon.NewServer(socketPath, eng)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Forward engine events to log output.
	go func() {
		for evt := range eng.Events() {
			switch e := evt.(type) {
			case engine.ServiceStateChanged:
				msg := fmt.Sprintf("[%s/%s] %s -> %s", e.Profile, e.ServiceName, e.From, e.To)
				if e.Error != nil {
					msg += fmt.Sprintf(" (%v)", e.Error)
				}
				log.Println(msg)
			case engine.LogEntry:
				log.Printf("[%s] [%s/%s] %s", e.Level, e.Profile, e.Service, e.Message)
			case engine.ProfileDone:
				log.Printf("[%s] all services stopped", e.Profile)
			}
		}
	}()

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		eng.Stop(context.Background(), nil)
		cancel()
	}()

	log.Printf("daemon starting on %s", socketPath)
	return srv.Serve(ctx)
}

// realProxyFactory implements engine.ProxyFactory using real TCP or sigv4 proxies.
type realProxyFactory struct{}

func (realProxyFactory) NewProxy(ctx context.Context, cfg engine.ProxyConfig) (engine.TCPProxy, error) {
	if cfg.Sigv4Service != "" {
		p := proxy.NewSigv4Proxy(proxy.Sigv4Config{
			ListenAddr: cfg.ListenAddr,
			TargetAddr: cfg.TargetAddr,
			RealHost:   cfg.RealHost,
			AWSProfile: cfg.AWSProfile,
			AWSRegion:  cfg.AWSRegion,
			AWSService: cfg.Sigv4Service,
		})
		if err := p.Start(ctx); err != nil {
			return nil, err
		}
		return p, nil
	}
	return proxy.StartNew(ctx, cfg.ListenAddr, cfg.TargetAddr)
}
