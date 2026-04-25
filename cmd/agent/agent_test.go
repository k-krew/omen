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

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// newTestAgent returns an agent configured for unit tests (no real tc execution).
func newTestAgent(token string) *agent {
	return &agent{
		port:        "9999",
		secretToken: token,
		log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

// --- buildTCArgs ---

func TestBuildTCArgs_LatencyOnly(t *testing.T) {
	args := buildTCArgs(faultRequest{LatencyMs: 100, Interface: "eth0"})
	want := []string{"qdisc", "add", "dev", "eth0", "root", "netem", "delay", "100ms"}
	assertSliceEqual(t, args, want)
}

func TestBuildTCArgs_LatencyAndJitter(t *testing.T) {
	args := buildTCArgs(faultRequest{LatencyMs: 100, JitterMs: 10, Interface: "eth0"})
	want := []string{"qdisc", "add", "dev", "eth0", "root", "netem", "delay", "100ms", "10ms"}
	assertSliceEqual(t, args, want)
}

func TestBuildTCArgs_PacketLossOnly(t *testing.T) {
	args := buildTCArgs(faultRequest{PacketLoss: 30, Interface: "eth0"})
	want := []string{"qdisc", "add", "dev", "eth0", "root", "netem", "loss", "30%"}
	assertSliceEqual(t, args, want)
}

func TestBuildTCArgs_LatencyAndPacketLoss(t *testing.T) {
	args := buildTCArgs(faultRequest{LatencyMs: 50, PacketLoss: 100, Interface: "eth0"})
	want := []string{"qdisc", "add", "dev", "eth0", "root", "netem", "delay", "50ms", "loss", "100%"}
	assertSliceEqual(t, args, want)
}

func TestBuildTCArgs_JitterIgnoredWithoutLatency(t *testing.T) {
	args := buildTCArgs(faultRequest{JitterMs: 10, PacketLoss: 50, Interface: "eth0"})
	// Jitter must be ignored when latency is 0.
	for _, a := range args {
		if strings.Contains(a, "ms") {
			t.Errorf("unexpected ms argument when latency is 0: %v", args)
		}
	}
}

// --- GET /healthz ---

func TestHandleHealthz(t *testing.T) {
	a := newTestAgent("")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	a.handleHealthz(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// --- Authentication middleware ---

func TestAuthenticate_CorrectToken(t *testing.T) {
	a := newTestAgent("secret")
	called := false
	handler := a.authenticate(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/network-fault", nil)
	req.Header.Set("X-Omen-Token", "secret")
	rr := httptest.NewRecorder()
	handler(rr, req)
	if !called {
		t.Error("expected handler to be called with correct token")
	}
}

func TestAuthenticate_WrongToken(t *testing.T) {
	a := newTestAgent("secret")
	called := false
	handler := a.authenticate(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	})
	req := httptest.NewRequest(http.MethodPost, "/network-fault", nil)
	req.Header.Set("X-Omen-Token", "wrong")
	rr := httptest.NewRecorder()
	handler(rr, req)
	if called {
		t.Error("expected handler NOT to be called with wrong token")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestAuthenticate_EmptySecretBypassesAuth(t *testing.T) {
	// When no secret is configured, all requests are allowed regardless of header.
	a := newTestAgent("")
	called := false
	handler := a.authenticate(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/network-fault", nil)
	// No X-Omen-Token header set.
	rr := httptest.NewRecorder()
	handler(rr, req)
	if !called {
		t.Error("expected handler to be called when no secret is configured")
	}
}

// --- POST /network-fault ---

func TestHandleFaultApply_InvalidJSON(t *testing.T) {
	a := newTestAgent("")
	req := httptest.NewRequest(http.MethodPost, "/network-fault", strings.NewReader("not-json"))
	rr := httptest.NewRecorder()
	a.handleFaultApply(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rr.Code)
	}
}

func TestHandleFaultApply_ZeroFault(t *testing.T) {
	a := newTestAgent("")
	body, _ := json.Marshal(faultRequest{LatencyMs: 0, PacketLoss: 0, Interface: "eth0"})
	req := httptest.NewRequest(http.MethodPost, "/network-fault", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	a.handleFaultApply(rr, req)
	// Must reject when neither latency nor packetLoss is set.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for zero-fault request, got %d", rr.Code)
	}
}

// --- DELETE /network-fault ---

// handleFaultRemove always returns 200 (rollback is best-effort).
// We can only test the response code here because tc is not available in CI.
func TestHandleFaultRemove_AlwaysReturns200(t *testing.T) {
	a := newTestAgent("")
	req := httptest.NewRequest(http.MethodDelete, "/network-fault?interface=eth0", nil)
	rr := httptest.NewRecorder()
	// tc will fail (not available), but the handler must still return 200.
	a.handleFaultRemove(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 even on tc failure, got %d", rr.Code)
	}
}

func TestHandleFaultRemove_DefaultInterface(t *testing.T) {
	a := newTestAgent("")
	// No interface query param — should default to eth0 without panicking.
	req := httptest.NewRequest(http.MethodDelete, "/network-fault", nil)
	rr := httptest.NewRecorder()
	a.handleFaultRemove(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// --- helpers ---

func assertSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("length mismatch: got %v, want %v", got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
