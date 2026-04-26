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
	"slices"
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

// --- buildTCCommands ---

// netemCmd extracts the netem qdisc command (always at index 1).
func netemCmd(t *testing.T, req faultRequest) []string {
	t.Helper()
	cmds := buildTCCommands(req, "9999")
	if len(cmds) < 2 {
		t.Fatalf("expected at least 2 commands, got %d", len(cmds))
	}
	return cmds[1]
}

func TestBuildTCCommands_StructureWithPort(t *testing.T) {
	cmds := buildTCCommands(faultRequest{LatencyMs: 100, Interface: "eth0"}, "9999")
	// Expect: prio, netem, agent-port filter, catch-all filter = 4 commands.
	if len(cmds) != 4 {
		t.Fatalf("expected 4 commands, got %d: %v", len(cmds), cmds)
	}
	// cmd[0] must set up the prio qdisc.
	assertContains(t, cmds[0], "prio")
	// cmd[1] must be the netem qdisc on parent 1:3.
	assertContains(t, cmds[1], "netem")
	assertContains(t, cmds[1], "1:3")
	// cmd[2] must filter the agent port to band 1:1.
	assertContains(t, cmds[2], "9999")
	assertContains(t, cmds[2], "1:1")
	// cmd[3] must redirect everything else to band 1:3.
	assertContains(t, cmds[3], "1:3")
}

func TestBuildTCCommands_StructureWithoutPort(t *testing.T) {
	cmds := buildTCCommands(faultRequest{LatencyMs: 100, Interface: "eth0"}, "")
	// Without agentPort: prio, netem, catch-all filter = 3 commands.
	if len(cmds) != 3 {
		t.Fatalf("expected 3 commands without port, got %d: %v", len(cmds), cmds)
	}
}

func TestBuildTCCommands_LatencyOnly(t *testing.T) {
	cmd := netemCmd(t, faultRequest{LatencyMs: 100, Interface: "eth0"})
	assertContains(t, cmd, "delay")
	assertContains(t, cmd, "100ms")
}

func TestBuildTCCommands_LatencyAndJitter(t *testing.T) {
	cmd := netemCmd(t, faultRequest{LatencyMs: 100, JitterMs: 10, Interface: "eth0"})
	assertContains(t, cmd, "delay")
	assertContains(t, cmd, "100ms")
	assertContains(t, cmd, "10ms")
}

func TestBuildTCCommands_PacketLossOnly(t *testing.T) {
	cmd := netemCmd(t, faultRequest{PacketLoss: 30, Interface: "eth0"})
	assertContains(t, cmd, "loss")
	assertContains(t, cmd, "30%")
}

func TestBuildTCCommands_LatencyAndPacketLoss(t *testing.T) {
	cmd := netemCmd(t, faultRequest{LatencyMs: 50, PacketLoss: 100, Interface: "eth0"})
	assertContains(t, cmd, "delay")
	assertContains(t, cmd, "50ms")
	assertContains(t, cmd, "loss")
	assertContains(t, cmd, "100%")
}

func TestBuildTCCommands_JitterIgnoredWithoutLatency(t *testing.T) {
	cmd := netemCmd(t, faultRequest{JitterMs: 10, PacketLoss: 50, Interface: "eth0"})
	// Jitter must be ignored when latency is 0.
	for _, arg := range cmd {
		if strings.Contains(arg, "ms") {
			t.Errorf("unexpected ms argument when latency is 0: %v", cmd)
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

func assertContains(t *testing.T, slice []string, elem string) {
	t.Helper()
	if !slices.Contains(slice, elem) {
		t.Errorf("expected %q in %v", elem, slice)
	}
}
