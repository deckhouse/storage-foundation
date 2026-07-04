/*
Copyright 2025 Flant JSC

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

package common

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const (
	// Job TTL for automatic cleanup after completion
	JobTTLSecondsAfterFinished = 30
)

type JobCfg struct {
	PodSpec               corev1.PodSpec
	JobName               types.NamespacedName // Name of Job to be created
	ResourceName          types.NamespacedName // DataImport or DataExport resource name
	LabelApplicationValue string               // DataImport or DataExport label value
}

// EnsureJob creates or updates a Job based on the configuration
func EnsureJob(ctx context.Context, client client.Client, cfg JobCfg) error {
	logger := log.FromContext(ctx).WithValues("jobNamespace", cfg.JobName.Namespace, "jobName", cfg.JobName.Name)
	logger.Info("Ensuring Job")

	oldJob, err := GetJob(ctx, client, cfg.JobName.Namespace, cfg.JobName.Name)
	if err != nil {
		return err
	}

	if oldJob == nil {
		// Job does not exist, create it
		logger.Info("Creating new Job")
		newJob := makeJob(cfg)
		err = client.Create(ctx, newJob)
		if err != nil {
			logger.Error(err, "Failed to create new Job")
			return err
		}

		logger.Info("Created new Job")
		return nil
	}

	// Jobs are immutable after creation, so we just return success
	logger.Info("Job already exists and is immutable")
	return nil
}

// GetJob retrieves a Job by namespace and name
func GetJob(ctx context.Context, client client.Client, namespace, name string) (*batchv1.Job, error) {
	logger := log.FromContext(ctx).WithValues("jobName", name)

	job := &batchv1.Job{}
	err := client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, job)
	if err != nil {
		if !kubeerrors.IsNotFound(err) {
			logger.Error(err, "Failed to get Job")
			return nil, err
		}

		// Job not found
		return nil, nil
	}

	return job, nil
}

// DeleteJob deletes a Job by namespace and name
// Returns:
// - bool: true if Job was deleted, false if it was not found
// - error: error if failed to delete Job
func DeleteJob(ctx context.Context, cl client.Client, name types.NamespacedName) (bool, error) {
	logger := log.FromContext(ctx).WithValues("jobNamespace", name.Namespace, "jobName", name.Name)
	logger.Info("Deleting Job")

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: name.Namespace,
			Name:      name.Name,
		},
	}

	propagationPolicy := metav1.DeletePropagationForeground
	err := cl.Delete(
		ctx,
		job,
		&client.DeleteOptions{PropagationPolicy: &propagationPolicy},
	)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			logger.Info("Job not found")
			return false, nil
		}

		logger.Error(err, "Failed to delete Job")
		return false, err
	}
	logger.Info("Job deleted")
	return true, nil
}

func IsJobCompleted(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func makeJob(cfg JobCfg) *batchv1.Job {
	ttlSeconds := int32(JobTTLSecondsAfterFinished)
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Job",
			APIVersion: "batch/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.JobName.Name,
			Namespace: cfg.JobName.Namespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: cfg.LabelApplicationValue, // To watch resource by controller
			},
			Annotations: map[string]string{
				// To identify which import/export resource is managing this job
				dev1alpha1.AnnotationStorageManagerNamespaceKey: cfg.ResourceName.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      cfg.ResourceName.Name,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttlSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						dev1alpha1.LabelApplicationKey: cfg.LabelApplicationValue, // To watch resource by controller
					},
					Annotations: map[string]string{
						// To identify which import/export resource is managing this job
						dev1alpha1.AnnotationStorageManagerNamespaceKey: cfg.ResourceName.Namespace,
						dev1alpha1.AnnotationStorageManagerNameKey:      cfg.ResourceName.Name,
					},
				},
				Spec: cfg.PodSpec,
			},
		},
	}

	return job
}
