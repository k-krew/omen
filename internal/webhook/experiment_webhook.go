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

// ExperimentValidator implements a validating webhook for Experiment.
type ExperimentValidator struct {
	ProtectedNamespaces []string
}

// SetupExperimentWebhookWithManager registers the validating webhook.
func SetupExperimentWebhookWithManager(mgr ctrl.Manager, protectedNamespaces []string) error {
	return ctrl.NewWebhookManagedBy(mgr, &chaosv1alpha1.Experiment{}).
		WithValidator(&ExperimentValidator{ProtectedNamespaces: protectedNamespaces}).
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
	var allErrs field.ErrorList

	specPath := field.NewPath("spec")

	modePath := specPath.Child("mode")
	hasCount := experiment.Spec.Mode.Count > 0
	hasPercent := experiment.Spec.Mode.Percent != nil

	// Exactly one of count or percent must be set.
	if !hasCount && !hasPercent {
		allErrs = append(allErrs, field.Required(modePath,
			"one of count or percent must be set"))
	}
	if hasCount && hasPercent {
		allErrs = append(allErrs, field.Invalid(modePath, nil,
			"count and percent are mutually exclusive"))
	}

	// Validate count when set.
	if hasCount {
		if experiment.Spec.Mode.Count < 1 {
			allErrs = append(allErrs, field.Invalid(
				modePath.Child("count"), experiment.Spec.Mode.Count,
				"must be at least 1",
			))
		}
		// Validate mode.count does not exceed safety.maxTargets.
		if experiment.Spec.Safety != nil && experiment.Spec.Safety.MaxTargets != nil {
			if experiment.Spec.Mode.Count > *experiment.Spec.Safety.MaxTargets {
				allErrs = append(allErrs, field.Invalid(
					modePath.Child("count"), experiment.Spec.Mode.Count,
					fmt.Sprintf("must not exceed safety.maxTargets (%d)", *experiment.Spec.Safety.MaxTargets),
				))
			}
		}
	}

	// Validate percent when set (marker enforces 1-100, but belt-and-suspenders).
	if hasPercent {
		if *experiment.Spec.Mode.Percent < 1 {
			allErrs = append(allErrs, field.Invalid(
				modePath.Child("percent"), *experiment.Spec.Mode.Percent,
				"must be at least 1 (0% is not allowed)",
			))
		}
	}

	// Validate cron schedule for Repeat policy.
	if experiment.Spec.RunPolicy.Type == chaosv1alpha1.RunPolicyRepeat {
		if experiment.Spec.RunPolicy.Schedule == "" {
			allErrs = append(allErrs, field.Required(
				specPath.Child("runPolicy", "schedule"),
				"schedule is required when runPolicy.type is Repeat",
			))
		} else {
			parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
			if _, err := parser.Parse(experiment.Spec.RunPolicy.Schedule); err != nil {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("runPolicy", "schedule"),
					experiment.Spec.RunPolicy.Schedule,
					fmt.Sprintf("invalid cron expression: %v", err),
				))
			}
		}
	}

	// Schedule is meaningless for Once policy.
	if experiment.Spec.RunPolicy.Type == chaosv1alpha1.RunPolicyOnce && experiment.Spec.RunPolicy.Schedule != "" {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("runPolicy", "schedule"),
			experiment.Spec.RunPolicy.Schedule,
			"schedule must not be set when runPolicy.type is Once",
		))
	}

	// Validate approval TTL requires approval.required=true.
	if experiment.Spec.Approval != nil {
		approvalPath := specPath.Child("approval")
		if !experiment.Spec.Approval.Required {
			if experiment.Spec.Approval.TTL != nil {
				allErrs = append(allErrs, field.Invalid(
					approvalPath.Child("ttl"), experiment.Spec.Approval.TTL,
					"ttl is only valid when approval.required is true",
				))
			}
			if experiment.Spec.Approval.Webhook != nil {
				allErrs = append(allErrs, field.Invalid(
					approvalPath.Child("webhook"), experiment.Spec.Approval.Webhook,
					"webhook is only valid when approval.required is true",
				))
			}
		}
	}

	// Reject experiments that target a protected namespace.
	if experiment.Spec.Selector.Namespace != "" {
		for _, protected := range v.ProtectedNamespaces {
			if experiment.Spec.Selector.Namespace == protected {
				allErrs = append(allErrs, field.Invalid(
					specPath.Child("selector", "namespace"),
					experiment.Spec.Selector.Namespace,
					fmt.Sprintf("namespace %q is protected and cannot be targeted by chaos experiments", protected),
				))
				break
			}
		}
	}

	if len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	return nil
}
