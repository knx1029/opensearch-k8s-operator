/*
Copyright 2021.

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

package controllers

import (
	"context"
	"time"

	"github.com/Opster/opensearch-k8s-operator/opensearch-operator/pkg/builders"
	"github.com/Opster/opensearch-k8s-operator/opensearch-operator/pkg/helpers"
	"github.com/Opster/opensearch-k8s-operator/opensearch-operator/pkg/reconcilers"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opsterv1 "github.com/Opster/opensearch-k8s-operator/opensearch-operator/api/v1"
)

// OpenSearchClusterReconciler reconciles a OpenSearchCluster object
type OpenSearchClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	MaxConcurrentReconciles int
}

//+kubebuilder:rbac:groups="opensearch.opster.io",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=opensearch.opster.io,resources=opensearchclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=opensearch.opster.io,resources=opensearchclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=opensearch.opster.io,resources=opensearchclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;create;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the OpenSearchCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *OpenSearchClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var logger = log.FromContext(ctx).WithValues("cluster", req.NamespacedName)
	logger.Info("Reconciling OpenSearchCluster")
	myFinalizerName := "Opster"

    var instance *opsterv1.OpenSearchCluster
    instance = &opsterv1.OpenSearchCluster{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// object not found, could have been deleted after
			// reconcile request, hence don't requeue
			return ctrl.Result{}, nil
		}
		// error reading the object, requeue the request
		return ctrl.Result{}, err
	}

	/// ------ check if CRD has been deleted ------ ///
	///	if ns deleted, delete the associated resources ///
	if instance.ObjectMeta.DeletionTimestamp.IsZero() {
		if !helpers.ContainsString(instance.GetFinalizers(), myFinalizerName) {
			// Use RetryOnConflict to update finalizer to handle any changes to resource
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
					return err
				}
				controllerutil.AddFinalizer(instance, myFinalizerName)
				return r.Update(ctx, instance)
			})
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		if helpers.ContainsString(instance.GetFinalizers(), myFinalizerName) {
			// our finalizer is present, so lets handle any external dependency
			if result, err := r.deleteExternalResources(ctx, instance, logger); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return result, err
			}

			// remove our finalizer from the list and update it.
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
					return err
				}
				controllerutil.RemoveFinalizer(instance, myFinalizerName)
				return r.Update(ctx, instance)
			})
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	/// if crd not deleted started phase 1
	if instance.Status.Phase == "" {
	    // instance.Status.Phase = opsterv1.PhasePending
	    // reconcilePhasePending does not do anything meaningful, other than setting the phase to Running
	    // so we can skip it and go directly to Running
        instance.Status.Phase = opsterv1.PhaseRunning
		instance.Status.ComponentsStatus = make([]opsterv1.ComponentStatus, 0)
	}

	switch instance.Status.Phase {
	case opsterv1.PhasePending:
		return r.reconcilePhasePending(ctx, instance, logger)
	case opsterv1.PhaseRunning:
		return r.reconcilePhaseRunning(ctx, instance, logger)
	default:
		// NOTHING WILL HAPPEN - DEFAULT
		return ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenSearchClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
	    WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		For(&opsterv1.OpenSearchCluster{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}

// delete associated cluster resources //
func (r *OpenSearchClusterReconciler) deleteExternalResources(ctx context.Context, instance *opsterv1.OpenSearchCluster, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("Deleting resources")
	// Run through all sub controllers to delete existing objects
	reconcilerContext := reconcilers.NewReconcilerContext(r.Recorder, instance, instance.Spec.NodePools)

	tls := reconcilers.NewTLSReconciler(
		r.Client,
		ctx,
		&reconcilerContext,
		instance,
	)
	securityconfig := reconcilers.NewSecurityconfigReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	config := reconcilers.NewConfigurationReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	cluster := reconcilers.NewClusterReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	dashboards := reconcilers.NewDashboardsReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)

	componentReconcilers := []reconcilers.ComponentReconciler{
		tls.DeleteResources,
		securityconfig.DeleteResources,
		config.DeleteResources,
		cluster.DeleteResources,
		dashboards.DeleteResources,
	}
	for _, rec := range componentReconcilers {
		result, err := rec()
		if err != nil || result.Requeue {
			return result, err
		}
	}
	logger.Info("Finished deleting resources")
	return ctrl.Result{}, nil
}

func (r *OpenSearchClusterReconciler) reconcilePhasePending(ctx context.Context, instance *opsterv1.OpenSearchCluster, logger logr.Logger) (ctrl.Result, error) {
	logger.Info("Start reconcile - Phase: PENDING")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
			return err
		}
		instance.Status.Phase = opsterv1.PhaseRunning
		instance.Status.ComponentsStatus = make([]opsterv1.ComponentStatus, 0)
		return r.Status().Update(ctx, instance)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *OpenSearchClusterReconciler) reconcilePhaseRunning(ctx context.Context, instance *opsterv1.OpenSearchCluster, logger logr.Logger) (ctrl.Result, error) {
	// Update initialized status first
	if !instance.Status.Initialized {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, client.ObjectKeyFromObject(instance), instance); err != nil {
				return err
			}
			instance.Status.Initialized = builders.AllMastersReady(ctx, r.Client, instance)
			return r.Status().Update(ctx, instance)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Run through all sub controllers to create or update all needed objects
	reconcilerContext := reconcilers.NewReconcilerContext(r.Recorder, instance, instance.Spec.NodePools)

	tls := reconcilers.NewTLSReconciler(
		r.Client,
		ctx,
		&reconcilerContext,
		instance,
	)
	securityconfig := reconcilers.NewSecurityconfigReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	config := reconcilers.NewConfigurationReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	cluster := reconcilers.NewClusterReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	scaler := reconcilers.NewScalerReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	dashboards := reconcilers.NewDashboardsReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	upgrade := reconcilers.NewUpgradeReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)
	restart := reconcilers.NewRollingRestartReconciler(
		r.Client,
		ctx,
		r.Recorder,
		&reconcilerContext,
		instance,
	)

	componentReconcilers := []reconcilers.ComponentReconciler{
		tls.Reconcile,
		securityconfig.Reconcile,
		config.Reconcile,
		cluster.Reconcile,
		scaler.Reconcile,
		dashboards.Reconcile,
		upgrade.Reconcile,
		restart.Reconcile,
	}
	for _, rec := range componentReconcilers {
		result, err := rec()
		if err != nil || result.Requeue {
			return result, err
		}
	}

	// -------- all resources has been created -----------
	return ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second}, nil
}
