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
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
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
	omenNamespaceEnv = "POD_NAMESPACE"
	omenAppLabel     = "app.kubernetes.io/name"
	omenAppName      = "omen"
)

// ExperimentReconciler reconciles a Experiment object
type ExperimentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experiments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experiments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experiments/finalizers,verbs=update
// +kubebuilder:rbac:groups=chaos.kreicer.dev,resources=experimentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ExperimentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	experiment := &chaosv1alpha1.Experiment{}
	if err := r.Get(ctx, req.NamespacedName, experiment); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if experiment.Spec.Paused {
		log.Info("experiment is paused, skipping scheduling")
		return ctrl.Result{}, nil
	}

	now := time.Now()

	// For Once policy: create exactly one run and execute immediately (no ExecuteAt).
	if experiment.Spec.RunPolicy.Type == chaosv1alpha1.RunPolicyOnce {
		if experiment.Status.LastScheduleTime != nil {
			return ctrl.Result{}, nil
		}
		return r.scheduleRun(ctx, experiment, now, time.Time{})
	}

	// Repeat policy: pre-create a run for the next cron tick immediately so
	// users can preview targets and approve before execution time arrives.
	if experiment.Spec.RunPolicy.Schedule == "" {
		log.Info("repeat experiment has no schedule, skipping")
		return ctrl.Result{}, nil
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(experiment.Spec.RunPolicy.Schedule)
	if err != nil {
		log.Error(err, "invalid cron schedule")
		return ctrl.Result{}, nil
	}

	var lastRun time.Time
	if experiment.Status.LastScheduleTime != nil {
		lastRun = experiment.Status.LastScheduleTime.Time
	} else {
		lastRun = experiment.CreationTimestamp.Time
	}

	// Find the next tick that is still in the future. If ticks were missed
	// (e.g. due to cooldown or a long-running previous run), advance past them.
	nextTick := schedule.Next(lastRun)
	if !nextTick.After(now) {
		nextTick = schedule.Next(now)
	}

	// If the experiment has an approval TTL and nextTick is closer than that
	// TTL (i.e. the user would have virtually no time to approve the very first
	// run), skip this immediate tick and schedule for the following one so the
	// full approval window is available.
	if experiment.Spec.Approval != nil && experiment.Spec.Approval.Required &&
		experiment.Spec.Approval.TTL != nil &&
		time.Until(nextTick) < experiment.Spec.Approval.TTL.Duration {
		nextTick = schedule.Next(nextTick)
	}

	result, err := r.scheduleRun(ctx, experiment, now, nextTick)
	if err != nil {
		return ctrl.Result{}, err
	}
	// If scheduleRun requests a specific requeue (e.g. cooldown), honour it.
	if result.RequeueAfter > 0 {
		return result, nil
	}

	// Requeue when the tick after nextTick is due, so we pre-create the
	// following run as soon as the current one finishes.
	nextAfterNext := schedule.Next(nextTick)
	return ctrl.Result{RequeueAfter: time.Until(nextAfterNext)}, nil
}

// scheduleRun creates an ExperimentRun for the given executeAt time.
// executeAt is zero for Once experiments (execute immediately) and the next
// cron tick for Repeat experiments (pre-created before the tick arrives).
func (r *ExperimentReconciler) scheduleRun(ctx context.Context, experiment *chaosv1alpha1.Experiment, now time.Time, executeAt time.Time) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Concurrency check: if there is an active run, skip.
	if experiment.Status.ActiveRun != "" {
		activeRun := &chaosv1alpha1.ExperimentRun{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: experiment.Namespace,
			Name:      experiment.Status.ActiveRun,
		}, activeRun)
		if err == nil && !isTerminalPhase(activeRun.Status.Phase) {
			log.Info("active run still in progress, concurrencyPolicy=Forbid, skipping")
			return ctrl.Result{}, nil
		}
		// Run is gone or terminal; clear it.
		patch := client.MergeFrom(experiment.DeepCopy())
		experiment.Status.ActiveRun = ""
		if err := r.Status().Patch(ctx, experiment, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Cooldown check is based on when the previous run was scheduled to execute,
	// not when it was created.
	if experiment.Spec.RunPolicy.Cooldown != nil && experiment.Status.LastScheduleTime != nil {
		cooldownEnd := experiment.Status.LastScheduleTime.Add(experiment.Spec.RunPolicy.Cooldown.Duration)
		if now.Before(cooldownEnd) {
			requeueAfter := cooldownEnd.Sub(now)
			log.Info("cooldown active, requeueing", "requeueAfter", requeueAfter)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	// Select targets.
	targets, err := r.selectTargets(ctx, experiment)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Derive a deterministic run name from the execution tick. Concurrent workers
	// racing on the same tick will produce the same name and the second will get
	// AlreadyExists, which is silently ignored.
	nameBase := executeAt
	if nameBase.IsZero() {
		nameBase = now
	}
	runName := fmt.Sprintf("%s-%d", experiment.Name, nameBase.Truncate(time.Minute).Unix())
	scheduledAt := metav1.NewTime(now)

	var execAtPtr *metav1.Time
	if !executeAt.IsZero() && executeAt.After(now) {
		t := metav1.NewTime(executeAt)
		execAtPtr = &t
	}

	run := &chaosv1alpha1.ExperimentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: experiment.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(experiment, chaosv1alpha1.GroupVersion.WithKind("Experiment")),
			},
		},
		Spec: chaosv1alpha1.ExperimentRunSpec{
			ExperimentName: experiment.Name,
			ExecuteAt:      execAtPtr,
		},
	}

	if len(targets) == 0 {
		if err := r.Create(ctx, run); err != nil {
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
			// A previous Status().Update() may have failed after Create succeeded.
			// Re-fetch the existing run; if it has no phase yet, repair its status.
			existing := &chaosv1alpha1.ExperimentRun{}
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, existing); getErr != nil {
				return ctrl.Result{}, getErr
			}
			if existing.Status.Phase != "" {
				return ctrl.Result{}, nil
			}
			run = existing
		}
		run.Status.Phase = chaosv1alpha1.PhaseSkipped
		run.Status.ScheduledAt = &scheduledAt
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(experiment, corev1.EventTypeNormal, "RunSkipped", "No eligible targets found")
		log.Info("no targets found, run marked as Skipped", "run", runName)
	} else {
		if err := r.Create(ctx, run); err != nil {
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
			// A previous Status().Update() may have failed after Create succeeded.
			// Re-fetch the existing run; if it has no phase yet, repair its status.
			existing := &chaosv1alpha1.ExperimentRun{}
			if getErr := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Name}, existing); getErr != nil {
				return ctrl.Result{}, getErr
			}
			if existing.Status.Phase != "" {
				return ctrl.Result{}, nil
			}
			run = existing
		}
		run.Status.Phase = chaosv1alpha1.PhasePreviewGenerated
		run.Status.PreviewTargets = targets
		run.Status.ScheduledAt = &scheduledAt
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Event(experiment, corev1.EventTypeNormal, "RunScheduled", fmt.Sprintf("ExperimentRun %s created with %d targets", runName, len(targets)))
		log.Info("ExperimentRun created", "run", runName, "targets", targets)
	}

	// LastScheduleTime tracks the executeAt time of the latest scheduled run so
	// that the next reconcile advances correctly to the tick after it.
	lastSchedule := metav1.NewTime(nameBase)
	patch := client.MergeFrom(experiment.DeepCopy())
	experiment.Status.LastScheduleTime = &lastSchedule
	experiment.Status.ActiveRun = runName
	if err := r.Status().Patch(ctx, experiment, patch); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.pruneHistory(ctx, experiment); err != nil {
		log.Error(err, "failed to prune run history")
	}

	return ctrl.Result{}, nil
}

func (r *ExperimentReconciler) pruneHistory(ctx context.Context, experiment *chaosv1alpha1.Experiment) error {
	var runList chaosv1alpha1.ExperimentRunList
	if err := r.List(ctx, &runList, client.InNamespace(experiment.Namespace)); err != nil {
		return err
	}

	var successful, failed []chaosv1alpha1.ExperimentRun
	for _, run := range runList.Items {
		if run.Spec.ExperimentName != experiment.Name {
			continue
		}
		switch run.Status.Phase {
		case chaosv1alpha1.PhaseCompleted:
			successful = append(successful, run)
		case chaosv1alpha1.PhaseFailed, chaosv1alpha1.PhaseSkipped, chaosv1alpha1.PhaseExpired:
			failed = append(failed, run)
		}
	}

	successLimit := int32(3)
	if experiment.Spec.SuccessfulHistoryLimit != nil {
		successLimit = *experiment.Spec.SuccessfulHistoryLimit
	}
	failLimit := int32(1)
	if experiment.Spec.FailedHistoryLimit != nil {
		failLimit = *experiment.Spec.FailedHistoryLimit
	}

	deleteOldest := func(runs []chaosv1alpha1.ExperimentRun, keep int32) error {
		sort.Slice(runs, func(i, j int) bool {
			return runs[i].CreationTimestamp.After(runs[j].CreationTimestamp.Time)
		})
		for i := int(keep); i < len(runs); i++ {
			if err := r.Delete(ctx, &runs[i]); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}

	if err := deleteOldest(successful, successLimit); err != nil {
		return err
	}
	return deleteOldest(failed, failLimit)
}

// selectTargets fetches pods matching the selector and applies all safety filters.
func (r *ExperimentReconciler) selectTargets(ctx context.Context, experiment *chaosv1alpha1.Experiment) ([]string, error) {
	log := logf.FromContext(ctx)

	sel := experiment.Spec.Selector
	listOpts := []client.ListOption{
		client.InNamespace(sel.Namespace),
		client.MatchingLabels(sel.Labels),
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, listOpts...); err != nil {
		return nil, err
	}

	// Determine Omen's own namespace for self-exclusion.
	omenNS := os.Getenv(omenNamespaceEnv)

	// Build deny namespace set.
	denyNS := map[string]struct{}{}
	if experiment.Spec.Safety != nil {
		for _, ns := range experiment.Spec.Safety.DenyNamespaces {
			denyNS[ns] = struct{}{}
		}
	}
	if omenNS != "" {
		denyNS[omenNS] = struct{}{}
	}

	var eligible []string
	for _, pod := range podList.Items {
		if _, denied := denyNS[pod.Namespace]; denied {
			continue
		}
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		// Self-exclusion: skip pods belonging to the Omen operator itself.
		if pod.Labels[omenAppLabel] == omenAppName {
			continue
		}
		eligible = append(eligible, pod.Name)
	}

	if len(eligible) == 0 {
		return nil, nil
	}

	// Determine count, respecting maxTargets.
	count := experiment.Spec.Mode.Count
	if experiment.Spec.Safety != nil && experiment.Spec.Safety.MaxTargets != nil {
		if count > *experiment.Spec.Safety.MaxTargets {
			count = *experiment.Spec.Safety.MaxTargets
		}
	}
	if count > len(eligible) {
		count = len(eligible)
	}

	// random selection - shuffle and take first N.
	shuffled := make([]string, len(eligible))
	copy(shuffled, eligible)
	rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	selected := shuffled[:count]

	// Sort for deterministic output in status.
	sort.Strings(selected)

	log.Info("targets selected", "count", len(selected), "eligible", len(eligible))
	return selected, nil
}

func isTerminalPhase(phase chaosv1alpha1.ExperimentRunPhase) bool {
	switch phase {
	case chaosv1alpha1.PhaseCompleted,
		chaosv1alpha1.PhaseFailed,
		chaosv1alpha1.PhaseSkipped,
		chaosv1alpha1.PhaseExpired:
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *ExperimentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("experiment-controller") //nolint:staticcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&chaosv1alpha1.Experiment{}).
		Owns(&chaosv1alpha1.ExperimentRun{}).
		Named("experiment").
		Complete(r)
}
