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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

var _ = Describe("Experiment Controller", func() {
	const (
		ns           = "default"
		experimentNS = "default"
	)

	ctx := context.Background()

	// The namespace must carry the chaos-enabled label for selectTargets to
	// include pods in it. Patch it once before any spec in this suite runs.
	BeforeEach(func() {
		defaultNS := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns}, defaultNS)).To(Succeed())
		patch := defaultNS.DeepCopy()
		if patch.Labels == nil {
			patch.Labels = map[string]string{}
		}
		patch.Labels[chaosv1alpha1.EnabledLabel] = chaosv1alpha1.EnabledLabelValue
		Expect(k8sClient.Update(ctx, patch)).To(Succeed())
	})

	newReconciler := func() *ExperimentReconciler {
		return &ExperimentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: &fakeEventRecorder{},
		}
	}

	makeRunningPod := func(name, namespace string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c", Image: "nginx"},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		}
	}

	Context("paused experiment", func() {
		It("does not create an ExperimentRun", func() {
			experiment := &chaosv1alpha1.Experiment{
				ObjectMeta: metav1.ObjectMeta{Name: "paused-exp", Namespace: ns},
				Spec: chaosv1alpha1.ExperimentSpec{
					Paused:    true,
					RunPolicy: chaosv1alpha1.RunPolicy{Type: chaosv1alpha1.RunPolicyOnce},
					Selector:  chaosv1alpha1.TargetSelector{Namespace: ns},
					Mode:      chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
					Action:    chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
				},
			}
			Expect(k8sClient.Create(ctx, experiment)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, experiment) })

			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "paused-exp", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			runList := &chaosv1alpha1.ExperimentRunList{}
			Expect(k8sClient.List(ctx, runList)).To(Succeed())
			Expect(runList.Items).To(BeEmpty())
		})
	})

	Context("Once policy with no eligible pods", func() {
		It("creates a Skipped ExperimentRun", func() {
			experiment := &chaosv1alpha1.Experiment{
				ObjectMeta: metav1.ObjectMeta{Name: "once-no-pods", Namespace: ns},
				Spec: chaosv1alpha1.ExperimentSpec{
					RunPolicy: chaosv1alpha1.RunPolicy{Type: chaosv1alpha1.RunPolicyOnce},
					Selector:  chaosv1alpha1.TargetSelector{Namespace: ns, Labels: map[string]string{"no-such": "label"}},
					Mode:      chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
					Action:    chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
				},
			}
			Expect(k8sClient.Create(ctx, experiment)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, experiment)
				runList := &chaosv1alpha1.ExperimentRunList{}
				_ = k8sClient.List(ctx, runList)
				for i := range runList.Items {
					_ = k8sClient.Delete(ctx, &runList.Items[i])
				}
			})

			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "once-no-pods", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			runList := &chaosv1alpha1.ExperimentRunList{}
			Expect(k8sClient.List(ctx, runList)).To(Succeed())
			Expect(runList.Items).To(HaveLen(1))
			Expect(runList.Items[0].Status.Phase).To(Equal(chaosv1alpha1.PhaseSkipped))
		})
	})

	Context("Once policy with eligible pods", func() {
		It("creates a PreviewGenerated ExperimentRun with targets", func() {
			pod := makeRunningPod("target-pod-1", ns, map[string]string{"app": "test-once"})
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			// Patch status to Running since envtest doesn't run kubelet.
			pod.Status.Phase = corev1.PodRunning
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })

			experiment := &chaosv1alpha1.Experiment{
				ObjectMeta: metav1.ObjectMeta{Name: "once-with-pods", Namespace: ns},
				Spec: chaosv1alpha1.ExperimentSpec{
					RunPolicy: chaosv1alpha1.RunPolicy{Type: chaosv1alpha1.RunPolicyOnce},
					Selector:  chaosv1alpha1.TargetSelector{Namespace: ns, Labels: map[string]string{"app": "test-once"}},
					Mode:      chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
					Action:    chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
				},
			}
			Expect(k8sClient.Create(ctx, experiment)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, experiment)
				runList := &chaosv1alpha1.ExperimentRunList{}
				_ = k8sClient.List(ctx, runList)
				for i := range runList.Items {
					_ = k8sClient.Delete(ctx, &runList.Items[i])
				}
			})

			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "once-with-pods", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			runList := &chaosv1alpha1.ExperimentRunList{}
			Expect(k8sClient.List(ctx, runList)).To(Succeed())
			Expect(runList.Items).To(HaveLen(1))
			Expect(runList.Items[0].Status.Phase).To(Equal(chaosv1alpha1.PhasePreviewGenerated))
			Expect(runList.Items[0].Status.PreviewTargets).To(ContainElement("target-pod-1"))
		})
	})

	Context("Once policy - idempotency", func() {
		It("does not create a second run on re-reconcile", func() {
			experiment := &chaosv1alpha1.Experiment{
				ObjectMeta: metav1.ObjectMeta{Name: "once-idempotent", Namespace: ns},
				Spec: chaosv1alpha1.ExperimentSpec{
					RunPolicy: chaosv1alpha1.RunPolicy{Type: chaosv1alpha1.RunPolicyOnce},
					Selector:  chaosv1alpha1.TargetSelector{Namespace: ns},
					Mode:      chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
					Action:    chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
				},
			}
			Expect(k8sClient.Create(ctx, experiment)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, experiment)
				runList := &chaosv1alpha1.ExperimentRunList{}
				_ = k8sClient.List(ctx, runList)
				for i := range runList.Items {
					_ = k8sClient.Delete(ctx, &runList.Items[i])
				}
			})

			r := newReconciler()
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "once-idempotent", Namespace: ns}}

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Re-fetch to get updated status.
			updated := &chaosv1alpha1.Experiment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "once-idempotent", Namespace: ns}, updated)).To(Succeed())

			// Second reconcile should return early because LastScheduleTime is set.
			runList := &chaosv1alpha1.ExperimentRunList{}
			Expect(k8sClient.List(ctx, runList)).To(Succeed())
			Expect(runList.Items).To(HaveLen(1))
		})
	})

	Context("cooldown enforcement", func() {
		It("skips scheduling when cooldown has not elapsed", func() {
			cooldown := metav1.Duration{Duration: 10 * time.Minute}
			lastSchedule := metav1.NewTime(time.Now().Add(-1 * time.Minute))

			experiment := &chaosv1alpha1.Experiment{
				ObjectMeta: metav1.ObjectMeta{Name: "cooldown-exp", Namespace: ns},
				Spec: chaosv1alpha1.ExperimentSpec{
					RunPolicy: chaosv1alpha1.RunPolicy{
						Type:     chaosv1alpha1.RunPolicyRepeat,
						Schedule: "* * * * *",
						Cooldown: &cooldown,
					},
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
					Action:   chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
				},
			}
			Expect(k8sClient.Create(ctx, experiment)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, experiment) })

			// Patch status to simulate a recent run.
			experiment.Status.LastScheduleTime = &lastSchedule
			Expect(k8sClient.Status().Update(ctx, experiment)).To(Succeed())

			r := newReconciler()
			result, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "cooldown-exp", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			runList := &chaosv1alpha1.ExperimentRunList{}
			Expect(k8sClient.List(ctx, runList)).To(Succeed())
			Expect(runList.Items).To(BeEmpty())
		})
	})

	Context("safety - maxTargets", func() {
		It("caps selected targets at maxTargets", func() {
			labels := map[string]string{"app": "max-targets-test"}
			for i := range 3 {
				p := makeRunningPod(fmt.Sprintf("maxtgt-pod-%d", i), ns, labels)
				Expect(k8sClient.Create(ctx, p)).To(Succeed())
				p.Status.Phase = corev1.PodRunning
				Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
				pod := p
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			}

			maxT := 1
			experiment := &chaosv1alpha1.Experiment{
				ObjectMeta: metav1.ObjectMeta{Name: "max-targets-exp", Namespace: ns},
				Spec: chaosv1alpha1.ExperimentSpec{
					RunPolicy: chaosv1alpha1.RunPolicy{Type: chaosv1alpha1.RunPolicyOnce},
					Selector:  chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:      chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 3},
					Action:    chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeDeletePod},
					Safety:    &chaosv1alpha1.Safety{MaxTargets: &maxT},
				},
			}
			Expect(k8sClient.Create(ctx, experiment)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, experiment)
				runList := &chaosv1alpha1.ExperimentRunList{}
				_ = k8sClient.List(ctx, runList)
				for i := range runList.Items {
					_ = k8sClient.Delete(ctx, &runList.Items[i])
				}
			})

			r := newReconciler()
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "max-targets-exp", Namespace: ns},
			})
			Expect(err).NotTo(HaveOccurred())

			runList := &chaosv1alpha1.ExperimentRunList{}
			Expect(k8sClient.List(ctx, runList)).To(Succeed())
			Expect(runList.Items).To(HaveLen(1))
			Expect(runList.Items[0].Status.PreviewTargets).To(HaveLen(1))
		})
	})
})
