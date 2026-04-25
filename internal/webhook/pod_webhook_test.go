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

package webhook_test

import (
	"context"
	"encoding/json"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
	omenwebhook "github.com/k-krew/omen/internal/webhook"
)

// makePodMutator builds a PodMutator backed by a fake client containing the
// supplied namespaces.
func makePodMutator(enabled bool, namespaces ...*corev1.Namespace) *omenwebhook.PodMutator {
	s := scheme.Scheme
	Expect(chaosv1alpha1.AddToScheme(s)).To(Succeed())

	objs := make([]runtime.Object, len(namespaces))
	for i, ns := range namespaces {
		objs[i] = ns
	}
	fc := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build()

	m := &omenwebhook.PodMutator{
		Client:      fc,
		AgentImage:  "ghcr.io/k-krew/omen-agent:latest",
		AgentPort:   9999,
		SecretToken: "test-secret",
		Enabled:     enabled,
	}
	Expect(m.InjectDecoder(admission.NewDecoder(s))).To(Succeed())
	return m
}

func podRequest(pod *corev1.Pod, ns string) admission.Request {
	raw, err := json.Marshal(pod)
	Expect(err).NotTo(HaveOccurred())
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Namespace: ns,
			Object:    runtime.RawExtension{Raw: raw},
			Operation: admissionv1.Create,
		},
	}
}

func enabledNS(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{chaosv1alpha1.EnabledLabel: chaosv1alpha1.EnabledLabelValue},
	}}
}

func plainNS(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func basicPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
}

// containerFromPatch decodes the injected container from any
// "add /spec/containers/N" patch (the library uses numeric indices, not "-").
func containerFromPatch(resp admission.Response) *corev1.Container {
	for _, p := range resp.Patches {
		if p.Operation != "add" {
			continue
		}
		// Match /spec/containers/N or /spec/containers/-
		if len(p.Path) < len("/spec/containers/") {
			continue
		}
		if p.Path[:len("/spec/containers/")] != "/spec/containers/" {
			continue
		}
		raw, err := json.Marshal(p.Value)
		Expect(err).NotTo(HaveOccurred())
		var c corev1.Container
		Expect(json.Unmarshal(raw, &c)).To(Succeed())
		return &c
	}
	return nil
}

var _ = Describe("PodMutator", func() {
	ctx := context.Background()
	const ns = "chaos-ns"
	const plain = "plain-ns"

	Describe("injection disabled (pre-flight registry check failed)", func() {
		It("allows the pod without any patches", func() {
			m := makePodMutator(false, enabledNS(ns))
			resp := m.Handle(ctx, podRequest(basicPod("p", ns), ns))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeNil())
		})
	})

	Describe("namespace label gate", func() {
		It("injects sidecar in a labeled namespace", func() {
			m := makePodMutator(true, enabledNS(ns))
			resp := m.Handle(ctx, podRequest(basicPod("p", ns), ns))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).NotTo(BeEmpty())
		})

		It("allows without patches in an unlabeled namespace", func() {
			m := makePodMutator(true, plainNS(plain))
			resp := m.Handle(ctx, podRequest(basicPod("p", plain), plain))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeNil())
		})

		It("allows without patches when namespace does not exist", func() {
			m := makePodMutator(true) // empty fake client
			resp := m.Handle(ctx, podRequest(basicPod("p", "ghost"), "ghost"))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeNil())
		})

		It("injects sidecar in a second chaos-enabled namespace", func() {
			const altNS = "other-chaos-ns"
			m := makePodMutator(true, enabledNS(ns), enabledNS(altNS))
			resp := m.Handle(ctx, podRequest(basicPod("worker", altNS), altNS))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).NotTo(BeEmpty())
		})
	})

	Describe("per-pod opt-out annotation", func() {
		It("skips injection for pods annotated chaos.kreicer.dev/ignore=true", func() {
			pod := basicPod("p", ns)
			pod.Annotations = map[string]string{chaosv1alpha1.IgnoreAnnotation: "true"}
			m := makePodMutator(true, enabledNS(ns))
			resp := m.Handle(ctx, podRequest(pod, ns))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeNil())
		})
	})

	Describe("idempotency", func() {
		It("does not inject a second sidecar when omen-agent already exists", func() {
			pod := basicPod("p", ns)
			pod.Spec.Containers = append(pod.Spec.Containers,
				corev1.Container{Name: omenwebhook.AgentContainerName, Image: "old-image"})
			m := makePodMutator(true, enabledNS(ns))
			resp := m.Handle(ctx, podRequest(pod, ns))
			Expect(resp.Allowed).To(BeTrue())
			Expect(resp.Patches).To(BeNil())
		})
	})

	Describe("injected container spec", func() {
		var injected *corev1.Container

		BeforeEach(func() {
			m := makePodMutator(true, enabledNS(ns))
			resp := m.Handle(ctx, podRequest(basicPod("p", ns), ns))
			Expect(resp.Allowed).To(BeTrue())
			injected = containerFromPatch(resp)
			Expect(injected).NotTo(BeNil(), "expected an add patch for /spec/containers/-")
		})

		It("uses the configured agent image", func() {
			Expect(injected.Image).To(Equal("ghcr.io/k-krew/omen-agent:latest"))
		})

		It("is named omen-agent", func() {
			Expect(injected.Name).To(Equal(omenwebhook.AgentContainerName))
		})

		It("has NET_ADMIN capability", func() {
			Expect(injected.SecurityContext).NotTo(BeNil())
			Expect(injected.SecurityContext.Capabilities).NotTo(BeNil())
			Expect(injected.SecurityContext.Capabilities.Add).To(ContainElement(corev1.Capability("NET_ADMIN")))
		})

		It("does not allow privilege escalation", func() {
			Expect(injected.SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*injected.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
		})

		It("sets resource requests and limits", func() {
			Expect(injected.Resources.Requests).NotTo(BeEmpty())
			Expect(injected.Resources.Limits).NotTo(BeEmpty())
		})

		It("injects OMEN_AGENT_PORT env var", func() {
			Expect(injected.Env).To(ContainElement(
				corev1.EnvVar{Name: "OMEN_AGENT_PORT", Value: "9999"},
			))
		})

		It("injects OMEN_SECRET_TOKEN env var", func() {
			Expect(injected.Env).To(ContainElement(
				corev1.EnvVar{Name: "OMEN_SECRET_TOKEN", Value: "test-secret"},
			))
		})

		It("has a liveness probe on /healthz", func() {
			Expect(injected.LivenessProbe).NotTo(BeNil())
			Expect(injected.LivenessProbe.HTTPGet).NotTo(BeNil())
			Expect(injected.LivenessProbe.HTTPGet.Path).To(Equal("/healthz"))
		})

		It("has a readiness probe on /healthz", func() {
			Expect(injected.ReadinessProbe).NotTo(BeNil())
			Expect(injected.ReadinessProbe.HTTPGet).NotTo(BeNil())
			Expect(injected.ReadinessProbe.HTTPGet.Path).To(Equal("/healthz"))
		})
	})

	Describe("malformed request", func() {
		It("returns HTTP 400 for invalid pod JSON", func() {
			m := makePodMutator(true, enabledNS(ns))
			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					Namespace: ns,
					Object:    runtime.RawExtension{Raw: []byte("not-valid-json")},
				},
			}
			resp := m.Handle(ctx, req)
			Expect(int(resp.Result.Code)).To(Equal(http.StatusBadRequest))
		})
	})
})
