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
	"maps"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("Target Selection and Safety Filters", func() {
	const ns = "selector-test-ns"
	// unlabeledNS is a namespace that deliberately lacks the chaos enabled label.
	const unlabeledNS = "selector-unlabeled-ns"

	ctx := context.Background()

	// ensureNamespace creates the namespace if it does not exist and then patches
	// it to ensure it has the given labels.
	ensureNamespace := func(name string, labels map[string]string) {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		_ = k8sClient.Create(ctx, namespace)
		// Patch the namespace labels regardless of whether Create succeeded.
		fetched := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, fetched)).To(Succeed())
		patch := fetched.DeepCopy()
		if patch.Labels == nil {
			patch.Labels = make(map[string]string)
		}
		maps.Copy(patch.Labels, labels)
		Expect(k8sClient.Update(ctx, patch)).To(Succeed())
	}

	BeforeEach(func() {
		ensureNamespace(ns, map[string]string{chaosv1alpha1.EnabledLabel: chaosv1alpha1.EnabledLabelValue})
		ensureNamespace(unlabeledNS, map[string]string{})
	})

	newReconciler := func() *ExperimentReconciler {
		return &ExperimentReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: &fakeEventRecorder{},
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

	makePodWithAgent := func(name, namespace string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c", Image: "nginx"},
					{Name: "omen-agent", Image: "ghcr.io/k-krew/omen-agent:latest"},
				},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = phase
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		return pod
	}

	Context("namespace opt-in label", func() {
		It("selects pods only from namespaces labeled chaos.kreicer.dev/enabled=true", func() {
			labels := map[string]string{"role": "ns-gate-test"}
			enabled := makePod("ns-gate-enabled", ns, labels, corev1.PodRunning)
			notEnabled := makePod("ns-gate-disabled", unlabeledNS, labels, corev1.PodRunning)
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, enabled)
				_ = k8sClient.Delete(ctx, notEnabled)
			})

			// Selector with no namespace restriction so both namespaces are queried.
			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 10},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(ConsistOf("ns-gate-enabled"))
		})

		It("returns nil when the target namespace has no enabled label", func() {
			labels := map[string]string{"role": "unlabeled-ns-test"}
			p := makePod("unlabeled-pod", unlabeledNS, labels, corev1.PodRunning)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, p) })

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: unlabeledNS, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 1},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			Expect(targets).To(BeNil())
		})
	})

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
			Expect(targets).To(ConsistOf("phase-running", "phase-pending", "phase-succeeded"))
		})
	})

	Context("network_fault pool filtering", func() {
		It("only selects pods with omen-agent container for network_fault actions", func() {
			labels := map[string]string{"role": "nf-pool-test"}
			withAgent := makePodWithAgent("nf-with-agent", ns, labels, corev1.PodRunning)
			withoutAgent := makePod("nf-without-agent", ns, labels, corev1.PodRunning)
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, withAgent)
				_ = k8sClient.Delete(ctx, withoutAgent)
			})

			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Count: 10},
					Action:   chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeNetworkFault},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			// Pod without omen-agent is in base (for percentage) but not in pool.
			Expect(targets).To(ConsistOf("nf-with-agent"))
		})

		It("uses full fleet size (base) for percentage, but selects only from pool", func() {
			labels := map[string]string{"role": "nf-pct-test"}
			// 4 pods total: 2 with agent, 2 without. 50% of 4 = 2 targets.
			// Both targets come from the agent pool (2 pods available).
			for i := range 2 {
				p := makePodWithAgent(fmt.Sprintf("nf-agent-%d", i), ns, labels, corev1.PodRunning)
				pod := p
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			}
			for i := range 2 {
				p := makePod(fmt.Sprintf("nf-noagent-%d", i), ns, labels, corev1.PodRunning)
				pod := p
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, pod) })
			}

			pct := 50
			exp := &chaosv1alpha1.Experiment{
				Spec: chaosv1alpha1.ExperimentSpec{
					Selector: chaosv1alpha1.TargetSelector{Namespace: ns, Labels: labels},
					Mode:     chaosv1alpha1.Mode{Type: chaosv1alpha1.ModeTypeRandom, Percent: &pct},
					Action:   chaosv1alpha1.Action{Type: chaosv1alpha1.ActionTypeNetworkFault},
				},
			}

			r := newReconciler()
			targets, err := r.selectTargets(ctx, exp)
			Expect(err).NotTo(HaveOccurred())
			// 50% of 4 (base) = 2 targets, selected from the agent pool (2 pods).
			Expect(targets).To(HaveLen(2))
			for _, t := range targets {
				Expect(t).To(HavePrefix("nf-agent-"))
			}
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
