package maroonedpods_operator

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"maroonedpods.io/maroonedpods/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

func (r *ReconcileMaroonedPods) watchMaroonedPodsCRD() error {
	if err := r.controller.Watch(&source.Kind{Type: &extv1.CustomResourceDefinition{}}, handler.EnqueueRequestsFromMapFunc(
		func(obj client.Object) []reconcile.Request {
			name := obj.GetName()
			if name != "mps.maroonpods.io" {
				return nil
			}
			cr, err := util.GetActiveMaroonedPods(r.client)
			if err != nil {
				return nil
			}
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Namespace: "",
						Name:      cr.Name,
					},
				},
			}
		},
	)); err != nil {
		return err
	}

	return nil
}
