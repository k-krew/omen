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

// ExperimentRunPhase represents the lifecycle stage of an ExperimentRun.
// +kubebuilder:validation:Enum=PendingPreview;PreviewGenerated;PendingApproval;Approved;Running;Completed;Failed;Skipped;Expired
type ExperimentRunPhase string

const (
	PhasePendingPreview   ExperimentRunPhase = "PendingPreview"
	PhasePreviewGenerated ExperimentRunPhase = "PreviewGenerated"
	PhasePendingApproval  ExperimentRunPhase = "PendingApproval"
	PhaseApproved         ExperimentRunPhase = "Approved"
	PhaseRunning          ExperimentRunPhase = "Running"
	PhaseCompleted        ExperimentRunPhase = "Completed"
	PhaseFailed           ExperimentRunPhase = "Failed"
	PhaseSkipped          ExperimentRunPhase = "Skipped"
	PhaseExpired          ExperimentRunPhase = "Expired"
)

// TargetResultStatus is the outcome of a single chaos action attempt.
// +kubebuilder:validation:Enum=Success;Failed
type TargetResultStatus string

const (
	TargetResultSuccess TargetResultStatus = "Success"
	TargetResultFailed  TargetResultStatus = "Failed"
)

// FailureReason is the taxonomy of known failure causes.
// +kubebuilder:validation:Enum=NotFound;AlreadyTerminated;PermissionDenied;Timeout;Unknown
type FailureReason string

const (
	ReasonNotFound          FailureReason = "NotFound"
	ReasonAlreadyTerminated FailureReason = "AlreadyTerminated"
	ReasonPermissionDenied  FailureReason = "PermissionDenied"
	ReasonTimeout           FailureReason = "Timeout"
	ReasonUnknown           FailureReason = "Unknown"
)

// TargetResult records the outcome of the chaos action for one target pod.
type TargetResult struct {
	// target is the name of the pod that was acted on.
	Target string `json:"target"`

	// status is the outcome of the action.
	Status TargetResultStatus `json:"status"`

	// reason is populated when status is Failed.
	// +optional
	Reason FailureReason `json:"reason,omitempty"`
}

// RunSummary aggregates the overall outcome of an ExperimentRun.
type RunSummary struct {
	// total is the number of targets that were acted on.
	Total int `json:"total"`

	// success is the count of successful actions.
	Success int `json:"success"`

	// failed is the count of failed actions.
	Failed int `json:"failed"`
}

// ExperimentRunSpec defines the desired state of ExperimentRun.
type ExperimentRunSpec struct {
	// experimentName is the name of the parent Experiment.
	// +kubebuilder:validation:MinLength=1
	ExperimentName string `json:"experimentName"`

	// approved must be patched to true by an external system to unblock execution
	// when the parent Experiment requires approval.
	// +kubebuilder:default=false
	// +optional
	Approved bool `json:"approved,omitempty"`
}

// ExperimentRunStatus defines the observed state of ExperimentRun.
type ExperimentRunStatus struct {
	// phase is the current lifecycle stage of this run.
	// +optional
	Phase ExperimentRunPhase `json:"phase,omitempty"`

	// previewTargets is the fixed list of pod names selected at preview time.
	// These targets are not recalculated during execution.
	// +optional
	PreviewTargets []string `json:"previewTargets,omitempty"`

	// results holds the per-target outcome after execution.
	// +optional
	Results []TargetResult `json:"results,omitempty"`

	// summary aggregates the execution outcomes.
	// +optional
	Summary *RunSummary `json:"summary,omitempty"`

	// scheduledAt records when this run was created by the scheduler.
	// +optional
	ScheduledAt *metav1.Time `json:"scheduledAt,omitempty"`

	// startedAt records when execution began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt records when the run reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=erun;eruns
// +kubebuilder:printcolumn:name="Experiment",type=string,JSONPath=`.spec.experimentName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Approved",type=boolean,JSONPath=`.spec.approved`
// +kubebuilder:printcolumn:name="ScheduledAt",type=date,JSONPath=`.status.scheduledAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExperimentRun is the Schema for the experimentruns API
type ExperimentRun struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ExperimentRun
	// +required
	Spec ExperimentRunSpec `json:"spec"`

	// status defines the observed state of ExperimentRun
	// +optional
	Status ExperimentRunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ExperimentRunList contains a list of ExperimentRun
type ExperimentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ExperimentRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExperimentRun{}, &ExperimentRunList{})
}
