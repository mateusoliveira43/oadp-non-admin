/*
Copyright 2024.

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

// Package controller contains all controllers of the project
package controller

import (
	"context"
	"errors"
	"reflect"

	"github.com/go-logr/logr"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nacv1alpha1 "github.com/migtools/oadp-non-admin/api/v1alpha1"
	"github.com/migtools/oadp-non-admin/internal/common/constant"
	"github.com/migtools/oadp-non-admin/internal/common/function"
	"github.com/migtools/oadp-non-admin/internal/handler"
	"github.com/migtools/oadp-non-admin/internal/predicate"
)

// NonAdminBackupReconciler reconciles a NonAdminBackup object
type NonAdminBackupReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	OADPNamespace string
}

type reconcileStepFunction func(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error)

const (
	phaseUpdateRequeue     = "NonAdminBackup - Requeue after Phase Update"
	conditionUpdateRequeue = "NonAdminBackup - Requeue after Condition Update"
	statusUpdateError      = "Failed to update NonAdminBackup Status"
)

// +kubebuilder:rbac:groups=nac.oadp.openshift.io,resources=nonadminbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nac.oadp.openshift.io,resources=nonadminbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nac.oadp.openshift.io,resources=nonadminbackups/finalizers,verbs=update

// +kubebuilder:rbac:groups=velero.io,resources=backups,verbs=get;list;watch;create;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state,
// defined in NonAdminBackup object Spec.
func (r *NonAdminBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.V(1).Info("NonAdminBackup Reconcile start")

	// Get the NonAdminBackup object
	nab := &nacv1alpha1.NonAdminBackup{}
	err := r.Get(ctx, req.NamespacedName, nab)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info(err.Error())
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch NonAdminBackup")
		return ctrl.Result{}, err
	}

	reconcileSteps := []reconcileStepFunction{
		r.init,
		r.validateSpec,
		r.syncVeleroBackupWithNonAdminBackup,
	}
	for _, step := range reconcileSteps {
		requeue, err := step(ctx, logger, nab)
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			return ctrl.Result{Requeue: true}, nil
		}
	}
	logger.V(1).Info("NonAdminBackup Reconcile exit")
	return ctrl.Result{}, nil
}

// init initializes the Status.Phase from the NonAdminBackup.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
//
// The function checks if the Phase of the NonAdminBackup object is empty.
// If it is empty, it sets the Phase to "New".
// It then returns boolean values indicating whether the reconciliation loop should requeue or exit
// and error value whether the status was updated successfully.
func (r *NonAdminBackupReconciler) init(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	if nab.Status.Phase == constant.EmptyString {
		updated := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminBackupPhaseNew)
		if updated {
			if err := r.Status().Update(ctx, nab); err != nil {
				logger.Error(err, statusUpdateError)
				return false, err
			}

			logger.V(1).Info(phaseUpdateRequeue)
			return true, nil
		}
	}

	logger.V(1).Info("NonAdminBackup Phase already initialized")
	return false, nil
}

// validateSpec validates the Spec from the NonAdminBackup.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
//
// The function validates the Spec from the NonAdminBackup object.
// If the BackupSpec is invalid, the function sets the NonAdminBackup phase to "BackingOff".
// If the BackupSpec is invalid, the function sets the NonAdminBackup condition Accepted to "False".
// If the BackupSpec is valid, the function sets the NonAdminBackup condition Accepted to "True".
func (r *NonAdminBackupReconciler) validateSpec(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	err := function.ValidateBackupSpec(nab)
	if err != nil {
		updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminBackupPhaseBackingOff)
		updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
			metav1.Condition{
				Type:    string(nacv1alpha1.NonAdminConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  "InvalidBackupSpec",
				Message: err.Error(),
			},
		)
		if updatedPhase || updatedCondition {
			if updateErr := r.Status().Update(ctx, nab); updateErr != nil {
				logger.Error(updateErr, statusUpdateError)
				return false, updateErr
			}
		}

		logger.Error(err, "NonAdminBackup Spec is not valid")
		return false, reconcile.TerminalError(err)
	}

	updated := meta.SetStatusCondition(&nab.Status.Conditions,
		metav1.Condition{
			Type:    string(nacv1alpha1.NonAdminConditionAccepted),
			Status:  metav1.ConditionTrue,
			Reason:  "BackupAccepted",
			Message: "backup accepted",
		},
	)
	if updated {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}

		logger.V(1).Info(conditionUpdateRequeue)
		return true, nil
	}

	logger.V(1).Info("NonAdminBackup Spec already validated")
	return false, nil
}

// syncVeleroBackupWithNonAdminBackup ensures the VeleroBackup associated with the given NonAdminBackup resource
// is created, if it does not exist.
// The function also updates the status and conditions of the NonAdminBackup resource to reflect the state
// of the VeleroBackup.
//
// Parameters:
//
//	ctx: Context for the request.
//	logger: Logger instance for logging messages.
//	nab: Pointer to the NonAdminBackup object.
func (r *NonAdminBackupReconciler) syncVeleroBackupWithNonAdminBackup(ctx context.Context, logger logr.Logger, nab *nacv1alpha1.NonAdminBackup) (bool, error) {
	veleroBackupName := function.GenerateVeleroBackupName(nab.Namespace, nab.Name)
	if veleroBackupName == constant.EmptyString {
		return false, errors.New("unable to generate Velero Backup name")
	}

	veleroBackup := velerov1.Backup{}
	veleroBackupLogger := logger.WithValues("VeleroBackup", types.NamespacedName{Name: veleroBackupName, Namespace: r.OADPNamespace})
	err := r.Get(ctx, client.ObjectKey{Namespace: r.OADPNamespace, Name: veleroBackupName}, &veleroBackup)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			veleroBackupLogger.Error(err, "Unable to fetch VeleroBackup")
			return false, err
		}
		// Create VeleroBackup
		veleroBackupLogger.Info("VeleroBackup not found")

		backupSpec := nab.Spec.BackupSpec.DeepCopy()
		backupSpec.IncludedNamespaces = []string{nab.Namespace}

		veleroBackup = velerov1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:        veleroBackupName,
				Namespace:   r.OADPNamespace,
				Labels:      function.GetNonAdminLabels(),
				Annotations: function.GetNonAdminBackupAnnotations(nab.ObjectMeta),
			},
			Spec: *backupSpec,
		}

		err = r.Create(ctx, &veleroBackup)
		if err != nil {
			veleroBackupLogger.Error(err, "Failed to create VeleroBackup")
			return false, err
		}
		veleroBackupLogger.Info("VeleroBackup successfully created")
	}

	updatedPhase := updateNonAdminPhase(&nab.Status.Phase, nacv1alpha1.NonAdminBackupPhaseCreated)
	updatedCondition := meta.SetStatusCondition(&nab.Status.Conditions,
		metav1.Condition{
			Type:    string(nacv1alpha1.NonAdminConditionQueued),
			Status:  metav1.ConditionTrue,
			Reason:  "BackupScheduled",
			Message: "Created Velero Backup object",
		},
	)
	updatedReference := updateNonAdminBackupVeleroBackupReference(&nab.Status, &veleroBackup)
	if updatedPhase || updatedCondition || updatedReference {
		if err := r.Status().Update(ctx, nab); err != nil {
			logger.Error(err, statusUpdateError)
			return false, err
		}

		logger.V(1).Info("NonAdminBackup - Exit after Status Update")
		return false, nil
	}

	// Ensure that the NonAdminBackup's NonAdminBackupStatus is in sync
	// with the VeleroBackup. Any required updates to the NonAdminBackup
	// Status will be applied based on the current state of the VeleroBackup.
	veleroBackupLogger.Info("VeleroBackup already exists, verifying if NonAdminBackup Status requires update")
	updated := updateNonAdminBackupVeleroBackupStatus(&nab.Status, &veleroBackup)
	if updated {
		if err := r.Status().Update(ctx, nab); err != nil {
			veleroBackupLogger.Error(err, "Failed to update NonAdminBackup Status after VeleroBackup reconciliation")
			return false, err
		}

		logger.V(1).Info("NonAdminBackup Status updated successfully")
	}

	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NonAdminBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nacv1alpha1.NonAdminBackup{}).
		WithEventFilter(predicate.CompositePredicate{
			NonAdminBackupPredicate: predicate.NonAdminBackupPredicate{},
			VeleroBackupPredicate: predicate.VeleroBackupPredicate{
				OADPNamespace: r.OADPNamespace,
			},
		}).
		// handler runs after predicate
		Watches(&velerov1.Backup{}, &handler.VeleroBackupHandler{}).
		Complete(r)
}

// updateNonAdminPhase sets the phase in NonAdminBackup object status and returns true
// if the phase is changed by this call.
func updateNonAdminPhase(phase *nacv1alpha1.NonAdminBackupPhase, newPhase nacv1alpha1.NonAdminBackupPhase) bool {
	// Ensure phase is valid
	if newPhase == constant.EmptyString {
		return false
	}

	if *phase == newPhase {
		return false
	}

	*phase = newPhase
	return true
}

// updateNonAdminBackupVeleroBackupStatus sets the VeleroBackup reference fields in NonAdminBackup object status and returns true
// if the VeleroBackup fields are changed by this call.
func updateNonAdminBackupVeleroBackupReference(status *nacv1alpha1.NonAdminBackupStatus, veleroBackup *velerov1.Backup) bool {
	if status.VeleroBackup == nil {
		status.VeleroBackup = &nacv1alpha1.VeleroBackup{
			Name:      veleroBackup.Name,
			Namespace: veleroBackup.Namespace,
		}
		return true
	} else if status.VeleroBackup.Name != veleroBackup.Name || status.VeleroBackup.Namespace != veleroBackup.Namespace {
		status.VeleroBackup.Name = veleroBackup.Name
		status.VeleroBackup.Namespace = veleroBackup.Namespace
		return true
	}
	return false
}

// updateNonAdminBackupVeleroBackupStatus sets the VeleroBackup status field in NonAdminBackup object status and returns true
// if the VeleroBackup fields are changed by this call.
func updateNonAdminBackupVeleroBackupStatus(status *nacv1alpha1.NonAdminBackupStatus, veleroBackup *velerov1.Backup) bool {
	if status.VeleroBackup == nil {
		status.VeleroBackup = &nacv1alpha1.VeleroBackup{
			Status: veleroBackup.Status.DeepCopy(),
		}
		return true
	} else if !reflect.DeepEqual(status.VeleroBackup.Status, &veleroBackup.Status) {
		status.VeleroBackup.Status = veleroBackup.Status.DeepCopy()
		return true
	}
	return false
}
