package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jarmocluyse/ads-go/pkg/ads"
)

type healthState struct {
	mu      sync.RWMutex
	ads     bool
	plc     bool
	tcState string // last known TwinCAT state string, e.g. "Run", "Config", "Stop"
}

func (h *healthState) setADS(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ads = v
	if !v {
		h.plc = false
		h.tcState = ""
	}
}

// setTCState is called from OnStateChange with the current ADS state string
// and whether the PLC is in Run mode.
func (h *healthState) setTCState(tcState string, plcRunning bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tcState = tcState
	h.plc = plcRunning
}

func (h *healthState) get() (adsOK, plcOK bool, tcState string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ads, h.plc, h.tcState
}

type healthResponse struct {
	Status  string `json:"status"`
	ADS     bool   `json:"ads"`
	PLC     bool   `json:"plc"`
	TCState string `json:"tc_state,omitempty"`
}

func writeHealth(w http.ResponseWriter, adsOK, plcOK bool, tcState string, code int) {
	status := "ok"
	if code != http.StatusOK {
		status = "down"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(healthResponse{ //nolint:errcheck
		Status:  status,
		ADS:     adsOK,
		PLC:     plcOK,
		TCState: tcState,
	})
}

func (h *healthState) handleHealth(w http.ResponseWriter, r *http.Request) {
	a, p, s := h.get()
	if a && p {
		writeHealth(w, a, p, s, http.StatusOK)
	} else {
		writeHealth(w, a, p, s, http.StatusServiceUnavailable)
	}
}

func (h *healthState) handleHealthADS(w http.ResponseWriter, r *http.Request) {
	a, p, s := h.get()
	if a {
		writeHealth(w, a, p, s, http.StatusOK)
	} else {
		writeHealth(w, a, p, s, http.StatusServiceUnavailable)
	}
}

func (h *healthState) handleHealthPLC(w http.ResponseWriter, r *http.Request) {
	a, p, s := h.get()
	if p {
		writeHealth(w, a, p, s, http.StatusOK)
	} else {
		writeHealth(w, a, p, s, http.StatusServiceUnavailable)
	}
}

func newHTTPServer(listen string, state *healthState) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", state.handleHealth)
	mux.HandleFunc("GET /health/ads", state.handleHealthADS)
	mux.HandleFunc("GET /health/plc", state.handleHealthPLC)
	return &http.Server{Addr: listen, Handler: mux}
}

// callUptimeKuma calls a push monitor endpoint, setting status, msg, and ping
// as query parameters on baseURL.  pingMs < 0 means no ping measurement available.
func callUptimeKuma(baseURL, status, msg string, pingMs int64) {
	u, err := url.Parse(baseURL)
	if err != nil {
		slog.Warn("webhook: invalid uptime-kuma URL", "url", baseURL, "error", err)
		return
	}
	q := url.Values{}
	q.Set("status", status)
	q.Set("msg", msg)
	if pingMs >= 0 {
		q.Set("ping", fmt.Sprintf("%d", pingMs))
	}
	u.RawQuery = q.Encode()
	target := u.String()

	resp, err := http.Get(target) //nolint:noctx
	if err != nil {
		slog.Warn("webhook: uptime-kuma request failed", "url", target, "error", err)
		return
	}
	resp.Body.Close()
	slog.Debug("webhook: uptime-kuma called", "url", target, "status_code", resp.StatusCode)
}

func callWebhookPost(rawURL string) {
	resp, err := http.Post(rawURL, "application/json", nil) //nolint:noctx
	if err != nil {
		slog.Warn("webhook: post request failed", "url", rawURL, "error", err)
		return
	}
	resp.Body.Close()
	slog.Debug("webhook: post called", "url", rawURL, "status_code", resp.StatusCode)
}

func runPushLoop(ctx context.Context, client *ads.Client, state *healthState, cfg Config) {
	if len(cfg.AdsWebhooks) == 0 && len(cfg.PlcWebhooks) == 0 {
		return
	}
	ticker := time.NewTicker(cfg.PushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			adsOK, plcOK, tcState := state.get()

			// Measure ADS round-trip only when connected.
			var pingMs int64 = -1
			if adsOK {
				start := time.Now()
				_, _ = client.ReadTcSystemState()
				pingMs = time.Since(start).Milliseconds()
			}

			for _, w := range cfg.AdsWebhooks {
				switch {
				case w.UptimeKuma != "":
					status, msg := adsUptimeKumaStatus(adsOK, tcState)
					go callUptimeKuma(w.UptimeKuma, status, msg, pingMs)
				case w.Post != "":
					go callWebhookPost(w.Post)
				}
			}

			for _, w := range cfg.PlcWebhooks {
				switch {
				case w.UptimeKuma != "":
					status, msg := plcUptimeKumaStatus(adsOK, plcOK, tcState)
					go callUptimeKuma(w.UptimeKuma, status, msg, pingMs)
				case w.Post != "":
					go callWebhookPost(w.Post)
				}
			}
		}
	}
}

func adsUptimeKumaStatus(adsOK bool, tcState string) (status, msg string) {
	if !adsOK {
		return "down", "ADS unreachable"
	}
	return "up", "TwinCAT " + tcState
}

func plcUptimeKumaStatus(adsOK, plcOK bool, tcState string) (status, msg string) {
	if !adsOK {
		return "down", "ADS unreachable"
	}
	if !plcOK {
		return "down", "TwinCAT " + tcState
	}
	return "up", "PLC running"
}

func runReconnectLoop(ctx context.Context, client *ads.Client, retryInterval time.Duration) {
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		slog.Info("reconnect: attempting", "attempt", attempt)
		if err := client.Connect(); err != nil {
			slog.Warn("reconnect: failed", "attempt", attempt, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
			continue
		}
		slog.Info("reconnect: succeeded", "attempts", attempt)
		return
	}
}
