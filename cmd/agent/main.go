/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// omen-agent runs as a sidecar inside target pods and exposes a small HTTP API
// that the controller uses to apply and remove Linux tc-netem network faults.
// The process is designed to never crash-loop: any error that occurs after
// startup is logged and the server keeps running so the pod stays Ready.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultPort      = "9999"
	defaultInterface = "eth0"
)

// faultRequest is the JSON body for POST /network-fault.
type faultRequest struct {
	LatencyMs  int64  `json:"latencyMs"`
	JitterMs   int64  `json:"jitterMs"`
	PacketLoss int    `json:"packetLoss"`
	Interface  string `json:"interface"`
}

type agent struct {
	port        string
	secretToken string
	log         *slog.Logger
}

// authenticate wraps a handler with token validation.
func (a *agent) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.secretToken != "" && r.Header.Get("X-Omen-Token") != a.secretToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleHealthz always returns 200 so the pod stays Ready even if tc is broken.
func (a *agent) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleFaultApply applies a tc-netem qdisc to the pod's network interface.
func (a *agent) handleFaultApply(w http.ResponseWriter, r *http.Request) {
	var req faultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Interface == "" {
		req.Interface = defaultInterface
	}
	if req.LatencyMs == 0 && req.PacketLoss == 0 {
		http.Error(w, "at least one of latencyMs or packetLoss must be non-zero", http.StatusBadRequest)
		return
	}

	args := buildTCArgs(req)
	if out, err := exec.CommandContext(r.Context(), "tc", args...).CombinedOutput(); err != nil {
		a.log.Error("Failed to apply network fault", "error", err, "output", string(out))
		http.Error(w, fmt.Sprintf("tc failed: %v: %s", err, out), http.StatusInternalServerError)
		return
	}
	a.log.Info("Network fault applied", "interface", req.Interface)
	w.WriteHeader(http.StatusOK)
}

// handleFaultRemove removes the tc-netem qdisc from the interface.
// It always returns 200 — rollback is best-effort; no qdisc means the fault
// was already gone (e.g. after a pod restart), which is acceptable.
func (a *agent) handleFaultRemove(w http.ResponseWriter, r *http.Request) {
	iface := r.URL.Query().Get("interface")
	if iface == "" {
		iface = defaultInterface
	}
	out, err := exec.CommandContext(r.Context(), "tc", "qdisc", "del", "dev", iface, "root").CombinedOutput()
	if err != nil {
		a.log.Warn("tc qdisc del returned error (may already be clean)", "error", err, "output", string(out))
	} else {
		a.log.Info("Network fault removed", "interface", iface)
	}
	w.WriteHeader(http.StatusOK)
}

// buildTCArgs translates a faultRequest into tc-qdisc-add arguments (without "tc").
func buildTCArgs(req faultRequest) []string {
	args := []string{"qdisc", "add", "dev", req.Interface, "root", "netem"}
	if req.LatencyMs > 0 {
		args = append(args, "delay", fmt.Sprintf("%dms", req.LatencyMs))
		if req.JitterMs > 0 {
			args = append(args, fmt.Sprintf("%dms", req.JitterMs))
		}
	}
	if req.PacketLoss > 0 {
		args = append(args, "loss", fmt.Sprintf("%d%%", req.PacketLoss))
	}
	return args
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	port := os.Getenv("OMEN_AGENT_PORT")
	if port == "" {
		port = defaultPort
	}
	secretToken := os.Getenv("OMEN_SECRET_TOKEN")

	a := &agent{port: port, secretToken: secretToken, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("POST /network-fault", a.authenticate(a.handleFaultApply))
	mux.HandleFunc("DELETE /network-fault", a.authenticate(a.handleFaultRemove))

	srv := &http.Server{
		Addr:              net.JoinHostPort("0.0.0.0", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Info("Starting omen-agent", "port", port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Zombie mode: log the error but keep the process alive so the pod stays Ready.
			log.Error("HTTP server stopped unexpectedly", "error", err)
		}
	}()

	<-ctx.Done()
	log.Info("Shutting down omen-agent")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("Graceful shutdown failed", "error", err)
	}
}
