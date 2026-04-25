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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

const (
	defaultWebhookTimeout = 10 * time.Second
	defaultApprovalTTL    = 10 * time.Minute
	deletionConcurrency   = 5
	// approvedPollInterval caps the RequeueAfter for runs waiting in Approved so
	// the controller recovers quickly after a VM pause or controller restart.
	approvedPollInterval = 30 * time.Second
	// missedExecutionGrace is the maximum time past ExecuteAt at which a run is
	// still allowed to execute. Beyond this window the run is marked Expired.
	missedExecutionGrace = 5 * time.Minute
	// defaultNetworkFaultDuration is used when no duration is specified in NetworkFaultSpec.
	defaultNetworkFaultDuration = 5 * time.Minute
	// networkFaultPollInterval caps RequeueAfter during active network fault duration.
	networkFaultPollInterval = 30 * time.Second
	// networkFaultFinalizer is added to ExperimentRuns executing a network_fault so
	// the controller can roll back the fault before the run object is deleted.
	networkFaultFinalizer = "chaos.omen.com/network-fault"
)

// ExperimentRunReconciler reconciles a ExperimentRun object
type ExperimentRunReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	Recorder            events.EventRecorder
	HTTPClient          *http.Client
	WebhookTimeout      time.Duration
	ControllerStartTime time.Time
	// AgentPort is the port on which omen-agent sidecars listen.
	AgentPort int
	// SecretToken is sent as X-Omen-Token in every request to the agent.
	SecretToken string
}

// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experimentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experimentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experimentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experiments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ExperimentRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	run := &chaosv1alpha1.ExperimentRun{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion: roll back any active network fault before allowing deletion.
	if !run.DeletionTimestamp.IsZero() {
		return r.handleRunDeletion(ctx, run)
	}

	// Terminal phases need no further action.
	if isTerminalPhase(run.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// Fetch parent Experiment — needed for both Running and later phases.
	experiment := &chaosv1alpha1.Experiment{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: run.Namespace,
		Name:      run.Spec.ExperimentName,
	}, experiment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Running phase handling depends on the action type.
	if run.Status.Phase == chaosv1alpha1.PhaseRunning {
		if experiment.Spec.Action.Type == chaosv1alpha1.ActionTypeNetworkFault {
			return r.handleNetworkFaultRollback(ctx, run, experiment)
		}
		// delete_pod: guard against Running phase surviving a controller restart.
		// A run whose StartedAt predates this process's start time was left in Running
		// by a previous process and must be marked Failed (split-brain prevention).
		if run.Status.StartedAt != nil && run.Status.StartedAt.Time.Before(r.ControllerStartTime) {
			log.Info("run found in Running phase from previous controller process, marking Failed")
			return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseFailed)
		}
		return ctrl.Result{}, nil
	}

	switch run.Status.Phase {
	case chaosv1alpha1.PhasePreviewGenerated:
		return r.handlePreviewGenerated(ctx, run, experiment)
	case chaosv1alpha1.PhasePendingApproval:
		return r.handlePendingApproval(ctx, run, experiment)
	case chaosv1alpha1.PhaseApproved:
		return r.handleApproved(ctx, run, experiment)
	case "":
		// Phase is not yet set by the parent ExperimentReconciler. Wait for the
		// status update to arrive; the informer will re-trigger this reconcile.
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// handlePreviewGenerated decides whether approval is needed or moves straight to execution.
func (r *ExperimentRunReconciler) handlePreviewGenerated(
	ctx context.Context,
	run *chaosv1alpha1.ExperimentRun,
	experiment *chaosv1alpha1.Experiment,
) (ctrl.Result, error) {
	// The ExperimentReconciler sets Phase and then calls Status().Update() in a
	// second request. If this reconcile was triggered by the Create event before
	// that update landed in the cache, PreviewTargets and ScheduledAt are still
	// empty. Requeue immediately so we pick up the completed status.
	if run.Status.ScheduledAt == nil || len(run.Status.PreviewTargets) == 0 {
		return ctrl.Result{Requeue: true}, nil
	}

	if experiment.Spec.Approval != nil && experiment.Spec.Approval.Required {
		if err := r.sendWebhook(ctx, run, experiment); err != nil {
			// Return the error so controller-runtime retries with exponential backoff.
			// The run only becomes Failed if it reaches the approval TTL while still
			// in PendingApproval, not on a transient network error here.
			r.Recorder.Eventf(run, nil, corev1.EventTypeWarning, "WebhookFailed", "WebhookFailed", "webhook delivery failed: %v", err)
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "AwaitingApproval", "AwaitingApproval", "waiting for manual approval")
		return r.transitionPhase(ctx, run, chaosv1alpha1.PhasePendingApproval)
	}
	// No approval needed - go straight to Approved then execute.
	return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseApproved)
}

// handlePendingApproval checks TTL expiry and spec.approved patch.
func (r *ExperimentRunReconciler) handlePendingApproval(
	ctx context.Context,
	run *chaosv1alpha1.ExperimentRun,
	experiment *chaosv1alpha1.Experiment,
) (ctrl.Result, error) {
	// Default TTL: time until the scheduled execution (for pre-created runs) or
	// the global default for immediate runs.
	ttl := defaultApprovalTTL
	if run.Spec.ExecuteAt != nil {
		maxTTL := run.Spec.ExecuteAt.Sub(run.CreationTimestamp.Time)
		if maxTTL > 0 {
			ttl = maxTTL
		}
	}
	// User-specified TTL is honoured only if it is shorter than the max above.
	if experiment.Spec.Approval != nil && experiment.Spec.Approval.TTL != nil {
		if userTTL := experiment.Spec.Approval.TTL.Duration; userTTL < ttl {
			ttl = userTTL
		}
	}

	var approvalStarted time.Time
	if run.Status.ScheduledAt != nil {
		approvalStarted = run.Status.ScheduledAt.Time
	} else {
		approvalStarted = run.CreationTimestamp.Time
	}

	expiry := approvalStarted.Add(ttl)
	now := time.Now()

	if now.After(expiry) {
		r.Recorder.Eventf(run, nil, corev1.EventTypeWarning, "RunExpired", "RunExpired", "approval TTL exceeded")
		return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseExpired)
	}

	if run.Spec.Approved {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "Approved", "Approved", "manual approval received")
		return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseApproved)
	}

	// Requeue just before TTL expiry to mark Expired promptly.
	return ctrl.Result{RequeueAfter: expiry.Sub(now)}, nil
}

// handleApproved transitions to Running and executes the chaos action.
func (r *ExperimentRunReconciler) handleApproved(
	ctx context.Context,
	run *chaosv1alpha1.ExperimentRun,
	experiment *chaosv1alpha1.Experiment,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// For pre-created runs, wait in Approved until the scheduled execution time.
	if run.Spec.ExecuteAt != nil {
		remaining := time.Until(run.Spec.ExecuteAt.Time)
		if remaining > 0 {
			// Cap the requeue at approvedPollInterval so the controller wakes up
			// frequently and recovers quickly from VM pauses or clock drift.
			log.Info("waiting for ExecuteAt", "remainingSeconds", remaining.Seconds())
			return ctrl.Result{RequeueAfter: min(remaining, approvedPollInterval)}, nil
		}
		// ExecuteAt is in the past. Allow a short grace window for normal jitter;
		// beyond that the run missed its execution slot and should not execute.
		if missed := -remaining; missed > missedExecutionGrace {
			log.Info("missed execution window, transitioning to Expired", "missedBy", missed.Round(time.Second))
			r.Recorder.Eventf(run, nil, corev1.EventTypeWarning, "MissedExecutionWindow", "MissedExecutionWindow",
				"execution time passed %s ago, skipping run", missed.Round(time.Second))
			return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseExpired)
		}
	}

	if _, err := r.transitionPhase(ctx, run, chaosv1alpha1.PhaseRunning); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "ExecutionStarted", "ExecutionStarted", "beginning chaos execution")

	if experiment.Spec.Action.Type == chaosv1alpha1.ActionTypeNetworkFault {
		return r.executeNetworkFault(ctx, run, experiment)
	}

	now := metav1.Now()
	run.Status.StartedAt = &now

	targets := run.Status.PreviewTargets
	results := make([]chaosv1alpha1.TargetResult, len(targets))

	// Execute pod deletions concurrently, bounded by deletionConcurrency.
	sem := make(chan struct{}, deletionConcurrency)
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(idx int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result := r.executeDeletePod(ctx, targetNamespace(experiment), t, experiment.Spec.DryRun, experiment.Spec.Action.Force)
			results[idx] = result
			log.Info("target action completed", "target", t, "status", result.Status, "reason", result.Reason)
		}(i, target)
	}
	wg.Wait()

	successCount := 0
	failedCount := 0
	for _, res := range results {
		if res.Status == chaosv1alpha1.TargetResultSuccess {
			successCount++
		} else {
			failedCount++
		}
	}

	completedAt := metav1.Now()
	run.Status.Results = results
	run.Status.Summary = &chaosv1alpha1.RunSummary{
		Total:   len(results),
		Success: successCount,
		Failed:  failedCount,
	}
	run.Status.CompletedAt = &completedAt

	finalPhase := chaosv1alpha1.PhaseCompleted
	if failedCount > 0 && successCount == 0 {
		finalPhase = chaosv1alpha1.PhaseFailed
	}

	run.Status.Phase = finalPhase
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "ExecutionFinished", "ExecutionFinished",
		"execution complete: total=%d success=%d failed=%d", len(results), successCount, failedCount)

	return ctrl.Result{}, nil
}

// executeDeletePod attempts to delete a single pod and returns the result.
func (r *ExperimentRunReconciler) executeDeletePod(ctx context.Context, namespace, podName string, dryRun bool, force bool) chaosv1alpha1.TargetResult {
	log := logf.FromContext(ctx)

	// Short-circuit: dry-run skips all real work including existence checks.
	if dryRun {
		log.Info("dryRun=true, skipping actual pod deletion", "pod", podName)
		return chaosv1alpha1.TargetResult{
			Target: podName,
			Status: chaosv1alpha1.TargetResultSuccess,
		}
	}

	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		if errors.IsNotFound(err) {
			return chaosv1alpha1.TargetResult{
				Target: podName,
				Status: chaosv1alpha1.TargetResultFailed,
				Reason: chaosv1alpha1.ReasonNotFound,
			}
		}
		return chaosv1alpha1.TargetResult{
			Target: podName,
			Status: chaosv1alpha1.TargetResultFailed,
			Reason: chaosv1alpha1.ReasonUnknown,
		}
	}

	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return chaosv1alpha1.TargetResult{
			Target: podName,
			Status: chaosv1alpha1.TargetResultFailed,
			Reason: chaosv1alpha1.ReasonAlreadyTerminated,
		}
	}

	var deleteOpts []client.DeleteOption
	if force {
		deleteOpts = append(deleteOpts, client.GracePeriodSeconds(0))
	}

	if err := r.Delete(ctx, pod, deleteOpts...); err != nil {
		if errors.IsNotFound(err) {
			return chaosv1alpha1.TargetResult{
				Target: podName,
				Status: chaosv1alpha1.TargetResultFailed,
				Reason: chaosv1alpha1.ReasonNotFound,
			}
		}
		if errors.IsForbidden(err) {
			return chaosv1alpha1.TargetResult{
				Target: podName,
				Status: chaosv1alpha1.TargetResultFailed,
				Reason: chaosv1alpha1.ReasonPermissionDenied,
			}
		}
		return chaosv1alpha1.TargetResult{
			Target: podName,
			Status: chaosv1alpha1.TargetResultFailed,
			Reason: chaosv1alpha1.ReasonUnknown,
		}
	}

	return chaosv1alpha1.TargetResult{
		Target: podName,
		Status: chaosv1alpha1.TargetResultSuccess,
	}
}

// executeNetworkFault sends HTTP fault requests to the omen-agent sidecar in each
// target pod, records Injected status, adds the rollback finalizer, and requeues
// after the configured fault duration.
func (r *ExperimentRunReconciler) executeNetworkFault(
	ctx context.Context,
	run *chaosv1alpha1.ExperimentRun,
	experiment *chaosv1alpha1.Experiment,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Add the finalizer FIRST, before touching any status fields.
	// r.Patch replaces the local run object with the server response (which has
	// no status fields), so any status written before the patch would be wiped.
	if !controllerutil.ContainsFinalizer(run, networkFaultFinalizer) {
		patch := client.MergeFrom(run.DeepCopy())
		controllerutil.AddFinalizer(run, networkFaultFinalizer)
		if err := r.Patch(ctx, run, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	now := metav1.Now()
	run.Status.StartedAt = &now

	nf := experiment.Spec.Action.NetworkFault
	targets := run.Status.PreviewTargets
	results := make([]chaosv1alpha1.TargetResult, len(targets))

	for i, target := range targets {
		if experiment.Spec.DryRun {
			results[i] = chaosv1alpha1.TargetResult{
				Target:            target,
				Status:            chaosv1alpha1.TargetResultSuccess,
				NetworkFaultPhase: chaosv1alpha1.NetworkFaultInjected,
			}
			log.Info("dryRun=true, skipping actual fault injection", "pod", target)
			continue
		}

		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: targetNamespace(experiment), Name: target}, pod); err != nil {
			results[i] = chaosv1alpha1.TargetResult{
				Target:            target,
				Status:            chaosv1alpha1.TargetResultFailed,
				Reason:            chaosv1alpha1.ReasonNotFound,
				NetworkFaultPhase: chaosv1alpha1.NetworkFaultFailed,
			}
			continue
		}

		if err := r.sendFaultRequest(ctx, pod, nf); err != nil {
			log.Error(err, "Failed to inject network fault via agent", "pod", target)
			results[i] = chaosv1alpha1.TargetResult{
				Target:            target,
				Status:            chaosv1alpha1.TargetResultFailed,
				Reason:            chaosv1alpha1.ReasonUnknown,
				NetworkFaultPhase: chaosv1alpha1.NetworkFaultFailed,
			}
			continue
		}

		results[i] = chaosv1alpha1.TargetResult{
			Target:            target,
			Status:            chaosv1alpha1.TargetResultSuccess,
			NetworkFaultPhase: chaosv1alpha1.NetworkFaultInjected,
		}
		log.Info("Network fault injected", "pod", target)
	}

	run.Status.Results = results
	run.Status.Summary = &chaosv1alpha1.RunSummary{Total: len(results)}

	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "FaultInjected", "FaultInjected",
		"network fault injected into %d target(s)", len(targets))

	duration := defaultNetworkFaultDuration
	if nf != nil && nf.Duration != nil {
		duration = nf.Duration.Duration
	}
	return ctrl.Result{RequeueAfter: duration}, nil
}

// handleNetworkFaultRollback is called on every reconcile while a network_fault run is
// in the Running phase. It waits for the fault duration to elapse, then sends rollback
// HTTP requests to each agent and transitions the run to Completed or Failed.
func (r *ExperimentRunReconciler) handleNetworkFaultRollback(
	ctx context.Context,
	run *chaosv1alpha1.ExperimentRun,
	experiment *chaosv1alpha1.Experiment,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if run.Status.StartedAt == nil {
		return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseFailed)
	}

	nf := experiment.Spec.Action.NetworkFault
	duration := defaultNetworkFaultDuration
	if nf != nil && nf.Duration != nil {
		duration = nf.Duration.Duration
	}

	elapsed := time.Since(run.Status.StartedAt.Time)
	if elapsed < duration {
		remaining := duration - elapsed
		return ctrl.Result{RequeueAfter: min(remaining, networkFaultPollInterval)}, nil
	}

	// Duration elapsed — roll back all injected targets.
	results := run.Status.Results
	successCount := 0
	failedCount := 0

	for i, res := range results {
		if res.NetworkFaultPhase == chaosv1alpha1.NetworkFaultCleanedUp {
			successCount++
			continue
		}
		if res.NetworkFaultPhase == chaosv1alpha1.NetworkFaultFailed {
			failedCount++
			continue
		}

		if experiment.Spec.DryRun {
			results[i].NetworkFaultPhase = chaosv1alpha1.NetworkFaultCleanedUp
			successCount++
			continue
		}

		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: targetNamespace(experiment), Name: res.Target}, pod); err != nil {
			// Pod is gone — network namespace is destroyed, fault is cleared.
			results[i].NetworkFaultPhase = chaosv1alpha1.NetworkFaultCleanedUp
			successCount++
			log.Info("Target pod gone during rollback, treating as cleaned up", "pod", res.Target)
			continue
		}

		if err := r.sendRollbackRequest(ctx, pod); err != nil {
			log.Error(err, "Failed to roll back network fault via agent", "pod", res.Target)
			results[i].Status = chaosv1alpha1.TargetResultFailed
			results[i].NetworkFaultPhase = chaosv1alpha1.NetworkFaultFailed
			failedCount++
			continue
		}

		results[i].NetworkFaultPhase = chaosv1alpha1.NetworkFaultCleanedUp
		successCount++
		log.Info("Network fault rolled back", "pod", res.Target)
	}

	completedAt := metav1.Now()
	run.Status.Results = results
	run.Status.Summary = &chaosv1alpha1.RunSummary{
		Total:   len(results),
		Success: successCount,
		Failed:  failedCount,
	}
	run.Status.CompletedAt = &completedAt

	finalPhase := chaosv1alpha1.PhaseCompleted
	if failedCount > 0 && successCount == 0 {
		finalPhase = chaosv1alpha1.PhaseFailed
	}
	run.Status.Phase = finalPhase

	// Persist the final status BEFORE removing the finalizer.
	// r.Patch replaces the local run object with the server response (no status
	// fields), so calling it first would wipe Results, Summary, CompletedAt and Phase.
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(run.DeepCopy())
	controllerutil.RemoveFinalizer(run, networkFaultFinalizer)
	if err := r.Patch(ctx, run, patch); err != nil {
		return ctrl.Result{}, err
	}

	eventType := corev1.EventTypeNormal
	if finalPhase == chaosv1alpha1.PhaseFailed {
		eventType = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(run, nil, eventType, "FaultRolledBack", "FaultRolledBack",
		"network fault rolled back: total=%d success=%d failed=%d", len(results), successCount, failedCount)
	r.Recorder.Eventf(run, nil, eventType, "PhaseTransition", "PhaseTransition", "phase changed to %s", finalPhase)

	return ctrl.Result{}, nil
}

// handleRunDeletion rolls back active network faults before the run is deleted.
func (r *ExperimentRunReconciler) handleRunDeletion(ctx context.Context, run *chaosv1alpha1.ExperimentRun) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(run, networkFaultFinalizer) {
		return ctrl.Result{}, nil
	}

	// Fetch the parent Experiment to resolve the target namespace.
	// If it's gone already, fall back to run.Namespace so we still attempt cleanup.
	targetNS := run.Namespace
	experiment := &chaosv1alpha1.Experiment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.ExperimentName}, experiment); err == nil {
		targetNS = targetNamespace(experiment)
	}

	for _, res := range run.Status.Results {
		if res.NetworkFaultPhase != chaosv1alpha1.NetworkFaultInjected {
			continue
		}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: targetNS, Name: res.Target}, pod); err != nil {
			continue
		}
		if err := r.sendRollbackRequest(ctx, pod); err != nil {
			log.Error(err, "Failed to roll back fault during ExperimentRun deletion", "pod", res.Target)
		}
	}

	patch := client.MergeFrom(run.DeepCopy())
	controllerutil.RemoveFinalizer(run, networkFaultFinalizer)
	return ctrl.Result{}, r.Patch(ctx, run, patch)
}

// agentURL returns the base URL for the omen-agent inside a pod.
func (r *ExperimentRunReconciler) agentURL(pod *corev1.Pod) string {
	port := r.AgentPort
	if port == 0 {
		port = 9999
	}
	return fmt.Sprintf("http://%s:%d", pod.Status.PodIP, port)
}

// sendFaultRequest posts a fault request to the agent inside the given pod.
func (r *ExperimentRunReconciler) sendFaultRequest(ctx context.Context, pod *corev1.Pod, nf *chaosv1alpha1.NetworkFaultSpec) error {
	body := map[string]any{"interface": "eth0"}
	if nf != nil {
		if nf.Latency != nil {
			body["latencyMs"] = nf.Latency.Milliseconds()
		}
		if nf.Jitter != nil {
			body["jitterMs"] = nf.Jitter.Milliseconds()
		}
		if nf.PacketLoss != nil {
			body["packetLoss"] = *nf.PacketLoss
		}
	}
	return r.agentRequest(ctx, http.MethodPost, r.agentURL(pod)+"/network-fault", body)
}

// sendRollbackRequest sends a DELETE to the agent to remove the active qdisc.
func (r *ExperimentRunReconciler) sendRollbackRequest(ctx context.Context, pod *corev1.Pod) error {
	return r.agentRequest(ctx, http.MethodDelete, r.agentURL(pod)+"/network-fault?interface=eth0", nil)
}

// agentRequest is a thin helper that marshals a body and performs an HTTP request
// to the omen-agent, setting the auth token header.
func (r *ExperimentRunReconciler) agentRequest(ctx context.Context, method, url string, body map[string]any) error {
	var reqBody *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.SecretToken != "" {
		req.Header.Set("X-Omen-Token", r.SecretToken)
	}

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("agent returned non-2xx status: %d", resp.StatusCode)
	}
	return nil
}

// sendWebhook posts the approval notification payload.
func (r *ExperimentRunReconciler) sendWebhook(ctx context.Context, run *chaosv1alpha1.ExperimentRun, experiment *chaosv1alpha1.Experiment) error {
	if experiment.Spec.Approval == nil || experiment.Spec.Approval.Webhook == nil {
		return nil
	}
	url := experiment.Spec.Approval.Webhook.URL
	if url == "" {
		return nil
	}

	scheduledAt := ""
	if run.Status.ScheduledAt != nil {
		scheduledAt = run.Status.ScheduledAt.Format(time.RFC3339)
	}

	payload := map[string]any{
		"experiment":  run.Spec.ExperimentName,
		"runName":     run.Name,
		"targets":     run.Status.PreviewTargets,
		"scheduledAt": scheduledAt,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	timeout := r.WebhookTimeout
	if timeout == 0 {
		timeout = defaultWebhookTimeout
	}

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned non-2xx status: %d", resp.StatusCode)
	}

	return nil
}

// transitionPhase updates the run's phase in status and emits a Kubernetes Event.
func (r *ExperimentRunReconciler) transitionPhase(
	ctx context.Context,
	run *chaosv1alpha1.ExperimentRun,
	phase chaosv1alpha1.ExperimentRunPhase,
) (ctrl.Result, error) {
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = phase
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		return ctrl.Result{}, err
	}

	eventType := corev1.EventTypeNormal
	if phase == chaosv1alpha1.PhaseFailed || phase == chaosv1alpha1.PhaseExpired {
		eventType = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(run, nil, eventType, "PhaseTransition", "PhaseTransition", "phase changed to %s", phase)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExperimentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.ControllerStartTime = time.Now()
	if r.HTTPClient == nil {
		timeout := r.WebhookTimeout
		if timeout == 0 {
			timeout = defaultWebhookTimeout
		}
		r.HTTPClient = &http.Client{Timeout: timeout}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&chaosv1alpha1.ExperimentRun{}).
		Named("experimentrun").
		Complete(r)
}
