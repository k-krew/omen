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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// IgnoreAnnotation is the pod annotation that opts a pod out of target selection.
	// Set to "true" on any pod to exclude it from all chaos experiments.
	IgnoreAnnotation = "chaos.kreicer.dev/ignore"
)

// RunPolicyType defines how often an Experiment executes.
// +kubebuilder:validation:Enum=Once;Repeat
type RunPolicyType string

const (
	RunPolicyOnce   RunPolicyType = "Once"
	RunPolicyRepeat RunPolicyType = "Repeat"
)

// ConcurrencyPolicy defines how overlapping runs are handled.
// +kubebuilder:validation:Enum=Forbid
type ConcurrencyPolicy string

const (
	ConcurrencyPolicyForbid ConcurrencyPolicy = "Forbid"
)

// ModeType defines the target selection strategy.
// +kubebuilder:validation:Enum=random
type ModeType string

const (
	ModeTypeRandom ModeType = "random"
)

// ActionType defines the chaos action to perform.
// +kubebuilder:validation:Enum=delete_pod
type ActionType string

const (
	ActionTypeDeletePod ActionType = "delete_pod"
)

// RunPolicy defines the scheduling policy for an Experiment.
type RunPolicy struct {
	// type specifies whether the experiment runs once or on a repeat schedule.
	// +kubebuilder:default=Once
	Type RunPolicyType `json:"type"`

	// schedule is a cron expression used when type is Repeat.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// cooldown is the minimum duration between consecutive runs.
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`

	// concurrencyPolicy defines what happens when a new run is triggered while one is still active.
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`
}

// TargetSelector defines which pods are eligible for selection.
type TargetSelector struct {
	// namespace restricts target selection to the specified namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// labels is a set of key/value pairs used to filter pods.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// Mode defines how many targets are selected.
type Mode struct {
	// type is the selection strategy.
	// +kubebuilder:default=random
	Type ModeType `json:"type"`

	// count is the fixed number of targets to select. Mutually exclusive with percent.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Count int `json:"count,omitempty"`

	// percent is the percentage of matching pods to select (1-100).
	// Mutually exclusive with count. The calculated value is always rounded up and
	// is guaranteed to be at least 1 pod even if the percentage yields a fraction.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	Percent *int `json:"percent,omitempty"`
}

// Action defines what chaos operation to perform.
type Action struct {
	// type is the chaos action to execute.
	Type ActionType `json:"type"`

	// force performs the action immediately without a grace period when true.
	// Only applicable for delete_pod actions.
	// +kubebuilder:default=false
	// +optional
	Force bool `json:"force,omitempty"`
}

// WebhookConfig holds the approval notification endpoint.
type WebhookConfig struct {
	// url is the HTTP endpoint to notify when approval is required.
	URL string `json:"url"`
}

// Approval defines the manual approval gate for an ExperimentRun.
type Approval struct {
	// required specifies whether manual approval is needed before execution.
	// +kubebuilder:default=false
	Required bool `json:"required"`

	// ttl is how long to wait for approval before the run expires.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// webhook configures the notification sent when approval is required.
	// +optional
	Webhook *WebhookConfig `json:"webhook,omitempty"`
}

// Safety defines the blast-radius constraints for an Experiment.
type Safety struct {
	// maxTargets is the hard limit on how many pods can be targeted in a single run.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTargets *int `json:"maxTargets,omitempty"`

	// denyNamespaces lists namespaces that must never be targeted.
	// +optional
	DenyNamespaces []string `json:"denyNamespaces,omitempty"`
}

// ExperimentSpec defines the desired state of Experiment.
type ExperimentSpec struct {
	// runPolicy controls scheduling behaviour.
	RunPolicy RunPolicy `json:"runPolicy"`

	// selector identifies the pool of pods eligible for targeting.
	Selector TargetSelector `json:"selector"`

	// mode defines the target selection strategy.
	Mode Mode `json:"mode"`

	// action defines the chaos operation to perform on selected targets.
	Action Action `json:"action"`

	// approval configures the optional manual approval gate.
	// +optional
	Approval *Approval `json:"approval,omitempty"`

	// safety defines the blast-radius constraints.
	// +optional
	Safety *Safety `json:"safety,omitempty"`

	// paused stops scheduling of new ExperimentRuns when true.
	// +kubebuilder:default=false
	// +optional
	Paused bool `json:"paused,omitempty"`

	// dryRun causes the controller to select targets and record them without
	// performing the actual chaos action.
	// +kubebuilder:default=false
	// +optional
	DryRun bool `json:"dryRun,omitempty"`

	// successfulHistoryLimit is the number of completed ExperimentRuns to retain.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	SuccessfulHistoryLimit *int32 `json:"successfulHistoryLimit,omitempty"`

	// failedHistoryLimit is the number of failed, skipped, and expired ExperimentRuns to retain.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedHistoryLimit *int32 `json:"failedHistoryLimit,omitempty"`
}

// ExperimentStatus defines the observed state of Experiment.
type ExperimentStatus struct {
	// conditions represent the lifecycle state of the Experiment.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// lastScheduleTime records when the most recent ExperimentRun was created.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// activeRun holds the name of the currently active ExperimentRun, if any.
	// +optional
	ActiveRun string `json:"activeRun,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=exp;exps
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.runPolicy.schedule`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="ActiveRun",type=string,JSONPath=`.status.activeRun`
// +kubebuilder:printcolumn:name="LastSchedule",type=string,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Experiment is the Schema for the experiments API
type Experiment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Experiment
	// +required
	Spec ExperimentSpec `json:"spec"`

	// status defines the observed state of Experiment
	// +optional
	Status ExperimentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ExperimentList contains a list of Experiment
type ExperimentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Experiment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Experiment{}, &ExperimentList{})
}
