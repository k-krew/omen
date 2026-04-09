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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

const (
	defaultWebhookTimeout = 10 * time.Second
	defaultApprovalTTL    = 10 * time.Minute
	deletionConcurrency   = 5
)

// ExperimentRunReconciler reconciles a ExperimentRun object
type ExperimentRunReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	Recorder            record.EventRecorder
	HTTPClient          *http.Client
	WebhookTimeout      time.Duration
	ControllerStartTime time.Time
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

	// Terminal phases need no further action.
	if isTerminalPhase(run.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// Guard against Running phase surviving a controller restart.
	// A run whose StartedAt predates this process's start time was left in Running
	// by a previous process and must be marked Failed (split-brain prevention).
	// A run whose StartedAt is after ControllerStartTime is executing in this process;
	// the concurrent reconcile triggered by the Running patch is safe to ignore.
	if run.Status.Phase == chaosv1alpha1.PhaseRunning {
		if run.Status.StartedAt != nil && run.Status.StartedAt.Time.Before(r.ControllerStartTime) {
			log.Info("run found in Running phase from previous controller process, marking Failed")
			return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseFailed)
		}
		return ctrl.Result{}, nil
	}

	// Fetch parent Experiment for approval config.
	experiment := &chaosv1alpha1.Experiment{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: run.Namespace,
		Name:      run.Spec.ExperimentName,
	}, experiment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "WebhookFailed", "webhook delivery failed: %v", err)
			return ctrl.Result{}, err
		}
		r.Recorder.Event(run, corev1.EventTypeNormal, "AwaitingApproval", "waiting for manual approval")
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
		r.Recorder.Event(run, corev1.EventTypeWarning, "RunExpired", "approval TTL exceeded")
		return r.transitionPhase(ctx, run, chaosv1alpha1.PhaseExpired)
	}

	if run.Spec.Approved {
		r.Recorder.Event(run, corev1.EventTypeNormal, "Approved", "manual approval received")
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
		if remaining := time.Until(run.Spec.ExecuteAt.Time); remaining > 0 {
			log.Info("waiting for ExecuteAt", "remainingSeconds", remaining.Seconds())
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}

	if _, err := r.transitionPhase(ctx, run, chaosv1alpha1.PhaseRunning); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(run, corev1.EventTypeNormal, "ExecutionStarted", "beginning chaos execution")

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
			result := r.executeDeletePod(ctx, run.Namespace, t, experiment.Spec.DryRun, experiment.Spec.Action.Force)
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

	r.Recorder.Eventf(run, corev1.EventTypeNormal, "ExecutionFinished",
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

// transitionPhase updates the run's phase in status and emits relevant events.
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
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExperimentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.ControllerStartTime = time.Now()
	r.Recorder = mgr.GetEventRecorderFor("experimentrun-controller") //nolint:staticcheck
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
