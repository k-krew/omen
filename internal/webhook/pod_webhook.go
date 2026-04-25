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

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

const (
	// AgentContainerName is the well-known name of the injected sidecar.
	AgentContainerName = "omen-agent"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=Ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=mpod.kb.io,admissionReviewVersions=v1
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list

// PodMutator injects the omen-agent sidecar into pods created in namespaces
// labeled with chaos.kreicer.dev/enabled=true.
type PodMutator struct {
	client.Client
	AgentImage  string
	AgentPort   int
	SecretToken string
	// Enabled is false when the pre-flight registry check fails; in that case
	// all admission requests are allowed without modification so user pods are
	// never bricked by an ImagePullBackOff.
	Enabled bool
	decoder admission.Decoder
}

// SetupPodWebhookWithManager registers the mutating webhook handler.
func SetupPodWebhookWithManager(mgr ctrl.Manager, m *PodMutator) {
	mgr.GetWebhookServer().Register("/mutate-v1-pod", &admission.Webhook{Handler: m})
}

// InjectDecoder satisfies admission.DecoderInjector.
func (m *PodMutator) InjectDecoder(d admission.Decoder) error {
	m.decoder = d
	return nil
}

// Handle is the core admission handler.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	if !m.Enabled {
		return admission.Allowed("injection disabled: agent image is not reachable")
	}

	pod := &corev1.Pod{}
	if err := m.decoder.DecodeRaw(req.Object, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check namespace opt-in label.
	ns := &corev1.Namespace{}
	if err := m.Get(ctx, types.NamespacedName{Name: req.Namespace}, ns); err != nil {
		// If we can't fetch the namespace, allow the pod without injection to
		// avoid blocking user workloads.
		return admission.Allowed("could not fetch namespace, skipping injection")
	}
	if ns.Labels[chaosv1alpha1.EnabledLabel] != chaosv1alpha1.EnabledLabelValue {
		return admission.Allowed("namespace not labeled for chaos injection")
	}

	// Respect the per-pod opt-out annotation.
	if pod.Annotations[chaosv1alpha1.IgnoreAnnotation] == "true" {
		return admission.Allowed("pod opted out via annotation")
	}

	// Idempotency guard.
	for _, c := range pod.Spec.Containers {
		if c.Name == AgentContainerName {
			return admission.Allowed("omen-agent already present")
		}
	}

	pod.Spec.Containers = append(pod.Spec.Containers, m.buildAgentContainer())

	marshaled, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

func (m *PodMutator) buildAgentContainer() corev1.Container {
	port := m.AgentPort
	if port == 0 {
		port = 9999
	}
	privileged := false
	allowEscalation := false
	return corev1.Container{
		Name:            AgentContainerName,
		Image:           m.AgentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{
			{Name: "agent", ContainerPort: int32(port), Protocol: corev1.ProtocolTCP},
		},
		Env: []corev1.EnvVar{
			{Name: "OMEN_AGENT_PORT", Value: fmt.Sprintf("%d", port)},
			{Name: "OMEN_SECRET_TOKEN", Value: m.SecretToken},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged:               &privileged,
			AllowPrivilegeEscalation: &allowEscalation,
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN"},
			},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(int32(port)),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(int32(port)),
				},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       10,
		},
	}
}
