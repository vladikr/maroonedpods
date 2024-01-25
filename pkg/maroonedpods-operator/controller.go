package maroonedpods_operator

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	mpcerts "maroonedpods.io/maroonedpods/pkg/maroonedpods-operator/resources/cert"
	"maroonedpods.io/maroonedpods/staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"

	"github.com/kelseyhightower/envconfig"
	mpcluster "maroonedpods.io/maroonedpods/pkg/maroonedpods-operator/resources/cluster"
	mpnamespaced "maroonedpods.io/maroonedpods/pkg/maroonedpods-operator/resources/namespaced"
	"kubevirt.io/controller-lifecycle-operator-sdk/pkg/sdk/callbacks"
	sdkr "kubevirt.io/controller-lifecycle-operator-sdk/pkg/sdk/reconciler"

	"maroonedpods.io/maroonedpods/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	finalizerName = "operator.maroonedpods.io"

	createVersionLabel = "operator.maroonedpods.io/createVersion"
	updateVersionLabel = "operator.maroonedpods.io/updateVersion"
	// LastAppliedConfigAnnotation is the annotation that holds the last resource state which we put on resources under our governance
	LastAppliedConfigAnnotation = "operator.maroonedpods.io/lastAppliedConfiguration"

	certPollInterval = 1 * time.Minute
)

var (
	log = logf.Log.WithName("maroonedpods-operator")
)

// Add creates a new MaroonedPods Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return r.add(mgr)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (*ReconcileMaroonedPods, error) {
	var namespacedArgs mpnamespaced.FactoryArgs
	namespace := util.GetNamespace()
	restClient := mgr.GetClient()

	clusterArgs := &mpcluster.FactoryArgs{
		Namespace: namespace,
		Client:    restClient,
		Logger:    log,
	}

	err := envconfig.Process("", &namespacedArgs)

	if err != nil {
		return nil, err
	}

	namespacedArgs.Namespace = namespace

	log.Info("", "VARS", fmt.Sprintf("%+v", namespacedArgs))

	scheme := mgr.GetScheme()
	uncachedClient, err := client.New(mgr.GetConfig(), client.Options{
		Scheme: scheme,
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		return nil, err
	}

	recorder := mgr.GetEventRecorderFor("operator-controller")

	r := &ReconcileMaroonedPods{
		client:         restClient,
		uncachedClient: uncachedClient,
		scheme:         scheme,
		recorder:       recorder,
		namespace:      namespace,
		clusterArgs:    clusterArgs,
		namespacedArgs: &namespacedArgs,
	}
	callbackDispatcher := callbacks.NewCallbackDispatcher(log, restClient, uncachedClient, scheme, namespace)
	r.reconciler = sdkr.NewReconciler(r, log, restClient, callbackDispatcher, scheme, createVersionLabel, updateVersionLabel, LastAppliedConfigAnnotation, certPollInterval, finalizerName, true, recorder)

	r.registerHooks()

	return r, nil
}

var _ reconcile.Reconciler = &ReconcileMaroonedPods{}

// ReconcileMaroonedPods reconciles a MaroonedPods object
type ReconcileMaroonedPods struct {
	// This Client, initialized using mgr.client() above, is a split Client
	// that reads objects from the cache and writes to the apiserver
	client client.Client

	// use this for getting any resources not in the install namespace or cluster scope
	uncachedClient client.Client
	scheme         *runtime.Scheme
	recorder       record.EventRecorder
	controller     controller.Controller

	namespace      string
	clusterArgs    *mpcluster.FactoryArgs
	namespacedArgs *mpnamespaced.FactoryArgs

	certManager CertManager
	reconciler  *sdkr.Reconciler
}

// SetController sets the controller dependency
func (r *ReconcileMaroonedPods) SetController(controller controller.Controller) {
	r.controller = controller
	r.reconciler.WithController(controller)
}

// Reconcile reads that state of the cluster for a MaroonedPods object and makes changes based on the state read
// and what is in the MaroonedPods.Spec
// Note:
// The Controller will requeue the request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileMaroonedPods) Reconcile(_ context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("request.Namespace", request.Namespace, "request.Name", request.Name)
	reqLogger.Info("Reconciling MaroonedPods CR")
	operatorVersion := r.namespacedArgs.OperatorVersion
	cr := &v1alpha1.MaroonedPods{}
	crKey := client.ObjectKey{Namespace: "", Name: request.NamespacedName.Name}
	err := r.client.Get(context.TODO(), crKey, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("MaroonedPods CR does not exist")
			return reconcile.Result{}, nil
		}
		reqLogger.Error(err, "Failed to get MaroonedPods object")
		return reconcile.Result{}, err
	}

	res, err := r.reconciler.Reconcile(request, operatorVersion, reqLogger)
	if err != nil {
		reqLogger.Error(err, "failed to reconcile")
	}
	return res, err
}

func (r *ReconcileMaroonedPods) add(mgr manager.Manager) error {
	// Create a new controller
	c, err := controller.New("maroonedpods-operator-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	r.SetController(c)

	if err = r.reconciler.WatchCR(); err != nil {
		return err
	}

	cm, err := NewCertManager(mgr, r.namespace)
	if err != nil {
		return err
	}

	r.certManager = cm

	return nil
}

// createOperatorConfig creates operator config map
func (r *ReconcileMaroonedPods) createOperatorConfig(cr client.Object) error {
	mpCR := cr.(*v1alpha1.MaroonedPods)
	installerLabels := util.GetRecommendedInstallerLabelsFromCr(mpCR)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      util.ConfigMapName,
			Namespace: r.namespace,
			Labels:    map[string]string{"operator.maroonedpods.io": ""},
		},
	}
	util.SetRecommendedLabels(cm, installerLabels, "maroonedpods-operator")

	if err := controllerutil.SetControllerReference(cr, cm, r.scheme); err != nil {
		return err
	}

	return r.client.Create(context.TODO(), cm)
}

func (r *ReconcileMaroonedPods) getConfigMap() (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Name: util.ConfigMapName, Namespace: r.namespace}

	if err := r.client.Get(context.TODO(), key, cm); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return cm, nil
}

func (r *ReconcileMaroonedPods) getCertificateDefinitions(mp *v1alpha1.MaroonedPods) []mpcerts.CertificateDefinition {
	args := &mpcerts.FactoryArgs{Namespace: r.namespace}

	if mp != nil && mp.Spec.CertConfig != nil {
		if mp.Spec.CertConfig.CA != nil {
			if mp.Spec.CertConfig.CA.Duration != nil {
				args.SignerDuration = &mp.Spec.CertConfig.CA.Duration.Duration
			}

			if mp.Spec.CertConfig.CA.RenewBefore != nil {
				args.SignerRenewBefore = &mp.Spec.CertConfig.CA.RenewBefore.Duration
			}
		}

		if mp.Spec.CertConfig.Server != nil {
			if mp.Spec.CertConfig.Server.Duration != nil {
				args.TargetDuration = &mp.Spec.CertConfig.Server.Duration.Duration
			}

			if mp.Spec.CertConfig.Server.RenewBefore != nil {
				args.TargetRenewBefore = &mp.Spec.CertConfig.Server.RenewBefore.Duration
			}
		}
	}

	return mpcerts.CreateCertificateDefinitions(args)
}
