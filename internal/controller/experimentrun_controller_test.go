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
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

var _ = Describe("ExperimentRun Controller", func() {
	const ns = "default"

	ctx := context.Background()

	newReconciler := func(httpClient *http.Client) *ExperimentRunReconciler {
		r := &ExperimentRunReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			Recorder:            &fakeEventRecorder{},
			ControllerStartTime: time.Now(),
		}
		if httpClient != nil {
			r.HTTPClient = httpClient
		}
		return r
	}

	makeExperiment := func(name string, approvalRequired bool) *chaosv1alpha1.Experiment {
		exp := &chaosv1alpha1.Experiment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: chaosv1alpha1.ExperimentSpec{
				RunPolicy: chaosv1alpha1.RunPolicy{Type: chaosv1alpha1.RunPolicyOnce},
				Selector:  chaosv1alpha1.TargetSelector{Namespace: ns},
				Mode:      chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
				Action:    chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
				DryRun:    true,
			},
		}
		if approvalRequired {
			ttl := metav1.Duration{Duration: 5 * time.Minute}
			exp.Spec.Approval = &chaosv1alpha1.Approval{Required: true, TTL: &ttl}
		}
		return exp
	}

	makeRun := func(name, experimentName string, phase chaosv1alpha1.ExperimentRunPhase, targets []string) *chaosv1alpha1.ExperimentRun {
		scheduledAt := metav1.Now()
		run := &chaosv1alpha1.ExperimentRun{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: chaosv1alpha1.ExperimentRunSpec{
				ExperimentName: experimentName,
			},
		}
		_ = k8sClient.Create(ctx, run)
		run.Status.Phase = phase
		run.Status.PreviewTargets = targets
		run.Status.ScheduledAt = &scheduledAt
		_ = k8sClient.Status().Update(ctx, run)
		return run
	}

	Context("terminal phase", func() {
		It("does nothing when run is already Completed", func() {
			exp := makeExperiment("exp-terminal", false)
			Expect(k8sClient.Create(ctx, exp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, exp) })

			run := makeRun("run-terminal", "exp-terminal", chaosv1alpha1.PhaseCompleted, nil)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

			r := newReconciler(nil)
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "run-terminal", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &chaosv1alpha1.ExperimentRun{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "run-terminal", Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(chaosv1alpha1.PhaseCompleted))
		})
	})

	Context("Running phase on restart", func() {
		It("marks the run as Failed to prevent split-brain", func() {
			exp := makeExperiment("exp-restart", false)
			Expect(k8sClient.Create(ctx, exp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, exp) })

			run := makeRun("run-restart", "exp-restart", chaosv1alpha1.PhaseRunning, nil)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

			// Simulate a run that was started before this controller process began.
			pastTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
			run.Status.StartedAt = &pastTime
			Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())

			r := newReconciler(nil)
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "run-restart", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &chaosv1alpha1.ExperimentRun{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "run-restart", Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(chaosv1alpha1.PhaseFailed))
		})
	})

	Context("no approval required", func() {
		It("transitions PreviewGenerated -> Approved -> Completed (dryRun)", func() {
			exp := makeExperiment("exp-no-approval", false)
			Expect(k8sClient.Create(ctx, exp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, exp) })

			run := makeRun("run-no-approval", "exp-no-approval", chaosv1alpha1.PhasePreviewGenerated, []string{"ghost-pod"})
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

			r := newReconciler(nil)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "run-no-approval", Namespace: ns}}

			// First pass: PreviewGenerated -> Approved
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Second pass: Approved -> Completed
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &chaosv1alpha1.ExperimentRun{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "run-no-approval", Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(chaosv1alpha1.PhaseCompleted))
			Expect(updated.Status.Summary).NotTo(BeNil())
			Expect(updated.Status.Summary.Total).To(Equal(1))
		})
	})

	Context("approval required - approved via spec patch", func() {
		It("transitions to Approved after spec.approved is set", func() {
			webhookCalled := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				webhookCalled = true
				w.WriteHeader(http.StatusOK)
			}))
			DeferCleanup(srv.Close)

			exp := makeExperiment("exp-approval", true)
			exp.Spec.Approval.Webhook = &chaosv1alpha1.WebhookConfig{URL: srv.URL}
			Expect(k8sClient.Create(ctx, exp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, exp) })

			run := makeRun("run-approval", "exp-approval", chaosv1alpha1.PhasePreviewGenerated, []string{"ghost-pod"})
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

			r := newReconciler(srv.Client())
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "run-approval", Namespace: ns}}

			// PreviewGenerated -> PendingApproval (webhook fires)
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(webhookCalled).To(BeTrue())

			updated := &chaosv1alpha1.ExperimentRun{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "run-approval", Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(chaosv1alpha1.PhasePendingApproval))

			// Simulate external approval patch.
			patch := updated.DeepCopy()
			patch.Spec.Approved = true
			Expect(k8sClient.Patch(ctx, patch, client.MergeFrom(updated))).To(Succeed())

			// PendingApproval -> Approved
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "run-approval", Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(chaosv1alpha1.PhaseApproved))
		})
	})

	Context("approval TTL expiry", func() {
		It("transitions to Expired when TTL elapses", func() {
			exp := makeExperiment("exp-ttl", true)
			ttl := metav1.Duration{Duration: 1 * time.Millisecond}
			exp.Spec.Approval.TTL = &ttl
			Expect(k8sClient.Create(ctx, exp)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, exp) })

			pastTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
			run := &chaosv1alpha1.ExperimentRun{
				ObjectMeta: metav1.ObjectMeta{Name: "run-ttl", Namespace: ns},
				Spec:       chaosv1alpha1.ExperimentRunSpec{ExperimentName: "exp-ttl"},
			}
			Expect(k8sClient.Create(ctx, run)).To(Succeed())
			run.Status.Phase = chaosv1alpha1.PhasePendingApproval
			run.Status.ScheduledAt = &pastTime
			Expect(k8sClient.Status().Update(ctx, run)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, run) })

			r := newReconciler(nil)
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "run-ttl", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &chaosv1alpha1.ExperimentRun{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "run-ttl", Namespace: ns}, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(chaosv1alpha1.PhaseExpired))
		})
	})
})
