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
	"fmt"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-chaos-kreicer-dev-v1alpha1-experiment,mutating=false,failurePolicy=fail,sideEffects=None,groups=chaos.kreicer.dev,resources=experiments,verbs=create;update,versions=v1alpha1,name=vexperiment.kb.io,admissionReviewVersions=v1

// ExperimentValidator implements a validating webhook for Experiment.
type ExperimentValidator struct{}

// SetupExperimentWebhookWithManager registers the validating webhook.
func SetupExperimentWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &chaosv1alpha1.Experiment{}).
		WithValidator(&ExperimentValidator{}).
		Complete()
}

// ValidateCreate validates a newly created Experiment.
func (v *ExperimentValidator) ValidateCreate(_ context.Context, experiment *chaosv1alpha1.Experiment) (admission.Warnings, error) {
	return nil, v.validateExperiment(experiment)
}

// ValidateUpdate validates an update to an existing Experiment.
func (v *ExperimentValidator) ValidateUpdate(_ context.Context, _, newExperiment *chaosv1alpha1.Experiment) (admission.Warnings, error) {
	return nil, v.validateExperiment(newExperiment)
}

// ValidateDelete is a no-op; deletions are always allowed.
func (v *ExperimentValidator) ValidateDelete(_ context.Context, _ *chaosv1alpha1.Experiment) (admission.Warnings, error) {
	return nil, nil
}

func (v *ExperimentValidator) validateExperiment(experiment *chaosv1alpha1.Experiment) error {
	specPath := field.NewPath("spec")
	allErrs := make(field.ErrorList, 0, 4)
	allErrs = append(allErrs, v.validateMode(experiment, specPath)...)
	allErrs = append(allErrs, v.validateRunPolicy(experiment, specPath)...)
	allErrs = append(allErrs, v.validateApproval(experiment, specPath)...)
	allErrs = append(allErrs, v.validateAction(experiment, specPath)...)

	if len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	return nil
}

func (v *ExperimentValidator) validateMode(experiment *chaosv1alpha1.Experiment, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	modePath := specPath.Child("mode")
	hasCount := experiment.Spec.Mode.Count > 0
	hasPercent := experiment.Spec.Mode.Percent != nil

	if !hasCount && !hasPercent {
		errs = append(errs, field.Required(modePath, "one of count or percent must be set"))
	}
	if hasCount && hasPercent {
		errs = append(errs, field.Invalid(modePath, nil, "count and percent are mutually exclusive"))
	}

	if hasCount {
		if experiment.Spec.Mode.Count < 1 {
			errs = append(errs, field.Invalid(modePath.Child("count"), experiment.Spec.Mode.Count, "must be at least 1"))
		}
		if experiment.Spec.Safety != nil && experiment.Spec.Safety.MaxTargets != nil &&
			experiment.Spec.Mode.Count > *experiment.Spec.Safety.MaxTargets {
			errs = append(errs, field.Invalid(
				modePath.Child("count"), experiment.Spec.Mode.Count,
				fmt.Sprintf("must not exceed safety.maxTargets (%d)", *experiment.Spec.Safety.MaxTargets),
			))
		}
	}

	if hasPercent && *experiment.Spec.Mode.Percent < 1 {
		errs = append(errs, field.Invalid(modePath.Child("percent"), *experiment.Spec.Mode.Percent,
			"must be at least 1 (0% is not allowed)"))
	}
	return errs
}

func (v *ExperimentValidator) validateRunPolicy(experiment *chaosv1alpha1.Experiment, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	schedulePath := specPath.Child("runPolicy", "schedule")

	if experiment.Spec.RunPolicy.Type == chaosv1alpha1.RunPolicyRepeat {
		if experiment.Spec.RunPolicy.Schedule == "" {
			errs = append(errs, field.Required(schedulePath, "schedule is required when runPolicy.type is Repeat"))
		} else {
			parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
			if _, err := parser.Parse(experiment.Spec.RunPolicy.Schedule); err != nil {
				errs = append(errs, field.Invalid(schedulePath, experiment.Spec.RunPolicy.Schedule,
					fmt.Sprintf("invalid cron expression: %v", err)))
			}
		}
	}

	if experiment.Spec.RunPolicy.Type == chaosv1alpha1.RunPolicyOnce && experiment.Spec.RunPolicy.Schedule != "" {
		errs = append(errs, field.Invalid(schedulePath, experiment.Spec.RunPolicy.Schedule,
			"schedule must not be set when runPolicy.type is Once"))
	}
	return errs
}

func (v *ExperimentValidator) validateApproval(experiment *chaosv1alpha1.Experiment, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	if experiment.Spec.Approval == nil || experiment.Spec.Approval.Required {
		return errs
	}
	approvalPath := specPath.Child("approval")
	if experiment.Spec.Approval.TTL != nil {
		errs = append(errs, field.Invalid(approvalPath.Child("ttl"), experiment.Spec.Approval.TTL,
			"ttl is only valid when approval.required is true"))
	}
	if experiment.Spec.Approval.Webhook != nil {
		errs = append(errs, field.Invalid(approvalPath.Child("webhook"), experiment.Spec.Approval.Webhook,
			"webhook is only valid when approval.required is true"))
	}
	return errs
}

func (v *ExperimentValidator) validateAction(experiment *chaosv1alpha1.Experiment, specPath *field.Path) field.ErrorList {
	var errs field.ErrorList
	actionPath := specPath.Child("action")

	switch experiment.Spec.Action.Type {
	case chaosv1alpha1.ActionTypeNetworkFault:
		nf := experiment.Spec.Action.NetworkFault
		if nf == nil {
			errs = append(errs, field.Required(actionPath.Child("networkFault"),
				"networkFault is required when action.type is network_fault"))
		} else {
			if nf.Latency == nil && nf.PacketLoss == nil {
				errs = append(errs, field.Invalid(actionPath.Child("networkFault"), nf,
					"at least one of latency or packetLoss must be set"))
			}
			if nf.Jitter != nil && nf.Latency == nil {
				errs = append(errs, field.Invalid(actionPath.Child("networkFault", "jitter"), nf.Jitter,
					"jitter requires latency to be set"))
			}
		}
	case chaosv1alpha1.ActionTypeDeletePod:
		if experiment.Spec.Action.NetworkFault != nil {
			errs = append(errs, field.Invalid(actionPath.Child("networkFault"), experiment.Spec.Action.NetworkFault,
				"networkFault must not be set when action.type is delete_pod"))
		}
	}
	return errs
}
