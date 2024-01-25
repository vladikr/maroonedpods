package maroonpods_operator

import (
	"context"
	"fmt"
	"maroonedpods.io/maroonedpods/staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// watch registers MaroonedPods-specific watches
func (r *ReconcileMaroonedPods) watch() error {
	if err := r.reconciler.WatchResourceTypes(&corev1.ConfigMap{}, &corev1.Secret{}); err != nil {
		return err
	}

	if err := r.watchMaroonedPodsCRD(); err != nil {
		return err
	}

	return nil
}

// preCreate creates the operator config map
func (r *ReconcileMaroonedPods) preCreate(cr client.Object) error {
	// claim the configmap
	if err := r.createOperatorConfig(cr); err != nil {
		return err
	}
	return nil
}

// checkSanity verifies whether config map exists and is in proper relation with the cr
func (r *ReconcileMaroonedPods) checkSanity(cr client.Object, reqLogger logr.Logger) (*reconcile.Result, error) {
	configMap, err := r.getConfigMap()
	if err != nil {
		return &reconcile.Result{}, err
	}
	if !metav1.IsControlledBy(configMap, cr) {
		ownerDeleted, err := r.configMapOwnerDeleted(configMap)
		if err != nil {
			return &reconcile.Result{}, err
		}

		if ownerDeleted || configMap.DeletionTimestamp != nil {
			reqLogger.Info("Waiting for maroonedpods-config to be deleted before reconciling", "MaroonedPods", cr.GetName())
			return &reconcile.Result{RequeueAfter: time.Second}, nil
		}

		reqLogger.Info("Reconciling to error state, unwanted MaroonedPods object")
		result, err := r.reconciler.ReconcileError(cr, "Reconciling to error state, unwanted MaroonedPods object")
		return &result, err
	}
	return nil, nil
}

// sync syncs certificates used by CDU
func (r *ReconcileMaroonedPods) sync(cr client.Object, logger logr.Logger) error {
	mp := cr.(*v1alpha1.MaroonedPods)
	if mp.DeletionTimestamp != nil {
		return nil
	}
	return r.certManager.Sync(r.getCertificateDefinitions(mp))
}

func (r *ReconcileMaroonedPods) configMapOwnerDeleted(cm *corev1.ConfigMap) (bool, error) {
	ownerRef := metav1.GetControllerOf(cm)
	if ownerRef != nil {
		if ownerRef.Kind != "MaroonedPods" {
			return false, fmt.Errorf("unexpected configmap owner kind %q", ownerRef.Kind)
		}

		owner := &v1alpha1.MaroonedPods{}
		if err := r.client.Get(context.TODO(), client.ObjectKey{Name: ownerRef.Name}, owner); err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}

			return false, err
		}

		if owner.DeletionTimestamp == nil && owner.UID == ownerRef.UID {
			return false, nil
		}
	}

	return true, nil
}

func (r *ReconcileMaroonedPods) registerHooks() {
	r.reconciler.
		WithPreCreateHook(r.preCreate).
		WithWatchRegistrator(r.watch).
		WithSanityChecker(r.checkSanity).
		WithPerishablesSynchronizer(r.sync)
}
