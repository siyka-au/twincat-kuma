package main

import (
	"context"
	flag "github.com/spf13/pflag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jarmocluyse/ads-go/pkg/ads"
	adsstateinfo "github.com/jarmocluyse/ads-go/pkg/ads/ads-stateinfo"
	adstypes "github.com/jarmocluyse/ads-go/pkg/ads/types"
)

func main() {
	configPath   := flag.String("config",          "", "Path to YAML config file (env: CONFIG)")
	targetNetID  := flag.String("target-net-id",   "", "AMS NetID of the TwinCAT runtime (env: ADS_TARGET_NET_ID)")
	routerHost   := flag.String("router-host",     "", "Hostname or IP of the AMS router (env: ADS_ROUTER_HOST)")
	routerPort   := flag.Int("router-port",          0, "TCP port of the AMS router (env: ADS_ROUTER_PORT)")
	timeout      := flag.Duration("timeout",         0, "Per-request read/write timeout (env: ADS_TIMEOUT)")
	listen       := flag.String("listen",           "", "HTTP listen address, e.g. :8080 (env: SERVER_LISTEN)")
	pollInterval := flag.Duration("poll-interval",   0, "ADS state polling interval (env: POLL_INTERVAL)")
	flag.Parse()

	cfg, err := LoadConfig(FlagOverrides{
		ConfigPath:   *configPath,
		TargetNetID:  *targetNetID,
		RouterHost:   *routerHost,
		RouterPort:   *routerPort,
		Timeout:      *timeout,
		Listen:       *listen,
		PollInterval: *pollInterval,
	})
	if err != nil {
		slog.Error("config: failed to load", "error", err)
		os.Exit(1)
	}

	slog.Info("starting twincat-kuma",
		"listen", cfg.Server.Listen,
		"target", cfg.Connection.TargetNetID,
		"router", cfg.Connection.RouterHost,
		"poll", cfg.PollInterval,
	)

	state := &healthState{}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// graceful tracks intentional disconnects so OnConnectionLost doesn't reconnect.
	var graceful bool
	var gracefulMu sync.Mutex

	// reconnecting prevents multiple concurrent reconnect loops.
	var reconnecting sync.Mutex

	settings := ads.ClientSettings{
		TargetNetID:                cfg.Connection.TargetNetID,
		RouterHost:                 cfg.Connection.RouterHost,
		RouterPort:                 cfg.Connection.RouterPort,
		Timeout:                    cfg.Connection.Timeout,
		StatePollingInterval:       cfg.PollInterval,
		MaxConsecutiveReadFailures: 2,
	}

	settings.OnConnect = func(client *ads.Client, addr ads.AmsAddress) error {
		slog.Info("ads: connected", "localAMS", addr.NetID, "port", addr.Port)
		state.setADS(true)
		return nil
	}

	settings.OnDisconnect = func(client *ads.Client) {
		slog.Info("ads: disconnected")
		state.setADS(false)
	}

	settings.OnConnectionLost = func(client *ads.Client, err error) {
		slog.Error("ads: connection lost", "error", err)
		state.setADS(false)

		gracefulMu.Lock()
		isGraceful := graceful
		gracefulMu.Unlock()
		if isGraceful {
			return
		}

		if !reconnecting.TryLock() {
			return
		}
		go func() {
			defer reconnecting.Unlock()
			runReconnectLoop(ctx, client, 5*time.Second)
		}()
	}

	settings.OnStateChange = func(client *ads.Client, newState, oldState *adsstateinfo.SystemState) {
		isRun := newState.AdsState == adstypes.ADSStateRun
		if oldState == nil {
			slog.Info("ads: initial state", "state", newState.AdsState.String(), "plc_running", isRun)
		} else {
			slog.Info("ads: state changed",
				"from", oldState.AdsState.String(),
				"to", newState.AdsState.String(),
				"plc_running", isRun,
			)
		}
		state.setTCState(newState.AdsState.String(), isRun)
	}

	client := ads.NewClient(settings, nil)
	if err := client.Connect(); err != nil {
		slog.Error("ads: initial connect failed", "error", err)
		os.Exit(1)
	}

	srv := newHTTPServer(cfg.Server.Listen, state)
	go func() {
		slog.Info("http: listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http: server error", "error", err)
		}
	}()

	go runPushLoop(ctx, client, state, cfg)

	<-ctx.Done()
	slog.Info("shutting down")

	gracefulMu.Lock()
	graceful = true
	gracefulMu.Unlock()

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("http: shutdown error", "error", err)
	}

	if err := client.Disconnect(); err != nil {
		slog.Error("ads: disconnect error", "error", err)
	}
}
