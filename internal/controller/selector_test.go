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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

var _ = Describe("Target Selection and Safety Filters", func() {
	const ns = "selector-test-ns"

	ctx := context.Background()

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		_ = k8sClient.Create(ctx, namespace)
	})

	newReconciler := func() *ExperimentReconciler {
		return &ExperimentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(10),
		}
	}

	makePod := func(name, namespace string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = phase
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}

	Context("label selector", func() {
		It("only returns pods matching the label selector", func() {
			labels := map[string]string{"role": "target"}
			p1 := makePod("sel-match-1", ns, labels, corev1.PodRunning)
			p2 := makePod("sel-match-2", ns, labels, corev1.PodRunning)
			_ = makePod("sel-no-match", ns, map[string]string{"role": "other"}, corev1.PodRunning)
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, p1)
				_ = k8sClient.Delete(ctx, p2)
				other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sel-no-match", Namespace: ns}}
				_ = k8sClient.Delete(ctx, other)
			})

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 10},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(ConsistOf("sel-match-1", "sel-match-2"))
		})
	})

	Context("non-running pods are included", func() {
		It("includes Pending and Succeeded pods in the eligible pool", func() {
			labels := map[string]string{"role": "phase-test"}
			running := makePod("phase-running", ns, labels, corev1.PodRunning)
			pending := makePod("phase-pending", ns, labels, corev1.PodPending)
			succeeded := makePod("phase-succeeded", ns, labels, corev1.PodSucceeded)
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, running)
				_ = k8sClient.Delete(ctx, pending)
				_ = k8sClient.Delete(ctx, succeeded)
			})

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 10},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			// All non-terminating pods are eligible regardless of phase so that
			// percentage calculations reflect the true fleet size.
			Expect(targets).To(ConsistOf("phase-running", "phase-pending", "phase-succeeded"))
		})
	})

	Context("denyNamespaces", func() {
		It("returns no targets when the selector namespace is denied", func() {
			labels := map[string]string{"role": "deny-test"}
			p := makePod("deny-pod", ns, labels, corev1.PodRunning)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, p) })

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
					Safety:   &chaosv1alpha1.Safety{DenyNamespaces: []string{ns}},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(BeEmpty())
		})
	})

	Context("self-exclusion", func() {
		It("excludes pods with the omen app label", func() {
			labels := map[string]string{"role": "self-excl", omenAppLabel: omenAppName}
			p := makePod("omen-pod", ns, labels, corev1.PodRunning)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, p) })

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: map[string]string{"role": "self-excl"}},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 10},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(BeEmpty())
		})
	})

	Context("count capping", func() {
		It("returns exactly mode.count targets when more are available", func() {
			labels := map[string]string{"role": "count-cap"}
			for i := range 5 {
				p := makePod(fmt.Sprintf("cap-pod-%d", i), ns, labels, corev1.PodRunning)
				pod := p
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			}

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 2},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(HaveLen(2))
		})
	})

	Context("maxTargets safety cap", func() {
		It("caps selection at maxTargets even when mode.count is higher", func() {
			labels := map[string]string{"role": "maxtgt"}
			for i := range 4 {
				p := makePod(fmt.Sprintf("maxtgt-pod-%d", i), ns, labels, corev1.PodRunning)
				pod := p
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			}

			maxT := 1
			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 4},
					Safety:   &chaosv1alpha1.Safety{MaxTargets: &maxT},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(HaveLen(1))
		})
	})

	Context("no eligible pods", func() {
		It("returns nil when no pods match", func() {
			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: map[string]string{"no": "match"}},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(BeNil())
		})
	})
})
