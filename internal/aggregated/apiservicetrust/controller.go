// Package apiservicetrust keeps the aggregated API server's APIService trusting its serving CA.
//
// The controller runs inside the aggregated API server process. It sets spec.caBundle of the
// v1alpha1.aggregation.coder.com APIService to the CA in the servingcert Secret and turns
// spec.insecureSkipTLSVerify off, in one merge patch and only when something differs. It reacts
// to a single-object informer on that APIService and to servingcert.Manager changes; it never
// polls, and failures never affect serving or readiness.
package apiservicetrust

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/coder/coder-k8s/internal/aggregated/servingcert"
)

const (
	// APIServiceName is the APIService registered by deploy/apiserver-apiservice.yaml.
	APIServiceName = "v1alpha1.aggregation.coder.com"
	// OptOutAnnotation set to "false" on the APIService makes the controller leave it alone, for
	// clusters where cert-manager or a GitOps tool owns spec.caBundle.
	OptOutAnnotation = "coder.com/manage-ca-bundle"
	// FieldManager identifies the controller's patches in managedFields.
	FieldManager = "coder-k8s-apiservice-cabundle"
	// DocsURL explains the RBAC, upgrade order, and opt-out.
	DocsURL = "https://coder.github.io/coder-k8s/how-to/deploy-aggregated-apiserver/#how-kube-apiserver-trusts-the-server"

	retryBaseDelay = 500 * time.Millisecond
	// retryMaxDelay bounds how long a repaired permission takes to be noticed.
	retryMaxDelay = 60 * time.Second
	// warnInterval rate-limits repeated warnings (for example while RBAC is missing).
	warnInterval = 5 * time.Minute
)

// APIServiceGVR is the apiregistration.k8s.io/v1 APIService resource. kube-aggregator's typed
// client is not vendored, so the controller uses the dynamic client.
var APIServiceGVR = schema.GroupVersionResource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"}

var log = ctrl.Log.WithName("apiservicetrust")

// Controller keeps the APIService caBundle in sync with the servingcert Secret.
type Controller struct {
	dyn       dynamic.Interface
	secrets   kubernetes.Interface
	namespace string
	now       func() time.Time

	queue    workqueue.TypedRateLimitingInterface[string]
	factory  dynamicinformer.DynamicSharedInformerFactory
	informer cache.SharedIndexInformer

	warnMu   sync.Mutex
	lastWarn map[string]time.Time
}

var _ dynamiccertificates.Listener = (*Controller)(nil)

// New returns a Controller for the servingcert Secret in namespace.
func New(dyn dynamic.Interface, secrets kubernetes.Interface, namespace string) (*Controller, error) {
	if dyn == nil || secrets == nil {
		return nil, fmt.Errorf("assertion failed: Kubernetes clients must not be nil")
	}
	if namespace == "" {
		return nil, fmt.Errorf("assertion failed: namespace must not be empty")
	}
	// A metadata.name field selector lets RBAC authorize list/watch with resourceNames.
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("metadata.name", APIServiceName).String()
	})
	c := &Controller{
		dyn:       dyn,
		secrets:   secrets,
		namespace: namespace,
		now:       time.Now,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](retryBaseDelay, retryMaxDelay),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "apiservice-cabundle"},
		),
		factory:  factory,
		informer: factory.ForResource(APIServiceGVR).Informer(),
		lastWarn: map[string]time.Time{},
	}
	if _, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.Enqueue() },
		UpdateFunc: func(any, any) { c.Enqueue() },
		DeleteFunc: func(any) { c.Enqueue() },
	}); err != nil {
		return nil, fmt.Errorf("add APIService event handler: %w", err)
	}
	return c, nil
}

// Enqueue schedules a sync. It implements dynamiccertificates.Listener, so registering the
// controller with servingcert.Manager.AddListener reacts to CA changes.
func (c *Controller) Enqueue() {
	c.queue.Add(APIServiceName)
}

// Run syncs until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	defer c.queue.ShutDown()
	c.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		return
	}
	c.Enqueue()
	go func() {
		<-ctx.Done()
		c.queue.ShutDown()
	}()
	for c.processNext(ctx) {
	}
	c.factory.Shutdown()
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)
	if err := c.sync(ctx); err != nil {
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// sync patches the APIService when its trust settings differ from the Secret's CA. It returns an
// error only for failures worth retrying.
func (c *Controller) sync(ctx context.Context) error {
	obj, exists, err := c.informer.GetStore().GetByKey(APIServiceName)
	if err != nil {
		return fmt.Errorf("read APIService from cache: %w", err)
	}
	if !exists {
		c.warn("absent", nil, "APIService is not registered; its caBundle will be set once it exists", "apiService", APIServiceName, "docs", DocsURL)
		return nil
	}
	apiService, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("assertion failed: APIService cache object is %T", obj)
	}
	if apiService.GetAnnotations()[OptOutAnnotation] == "false" {
		c.warn("optout", nil, "APIService opted out of caBundle management; leaving it unchanged", "annotation", OptOutAnnotation+"=false")
		return nil
	}

	secret, err := c.secrets.CoreV1().Secrets(c.namespace).Get(ctx, servingcert.SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Mid-rotation: the serving certificate manager creates the Secret and notifies us.
		c.warn("nosecret", nil, "serving CA Secret not found; not changing the APIService caBundle", "secret", c.namespace+"/"+servingcert.SecretName)
		return nil
	}
	if err != nil {
		c.warn("getsecret", err, "cannot read the serving CA Secret; retrying", "secret", c.namespace+"/"+servingcert.SecretName)
		return err
	}
	// Only validated trust material is ever written: the API does not validate caBundle.
	bundle, err := servingcert.Parse(secret, c.namespace, c.now())
	if err != nil {
		c.warn("corrupt", err, "serving CA Secret is invalid; not changing the APIService caBundle")
		return nil
	}

	want := base64.StdEncoding.EncodeToString(bundle.CACertPEM)
	have, _, _ := unstructured.NestedString(apiService.Object, "spec", "caBundle")
	insecure, _, _ := unstructured.NestedBool(apiService.Object, "spec", "insecureSkipTLSVerify")
	if have == want && !insecure {
		return nil
	}

	// One patch: the API rejects insecureSkipTLSVerify=true together with a caBundle.
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"caBundle": want, "insecureSkipTLSVerify": false}})
	if err != nil {
		return fmt.Errorf("assertion failed: marshal patch: %w", err)
	}
	if _, err := c.dyn.Resource(APIServiceGVR).Patch(ctx, APIServiceName, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		if apierrors.IsForbidden(err) {
			c.warn("forbidden", err, "missing permission to patch the APIService caBundle; apply config/rbac/apiservice-cabundle-role.yaml",
				"permission", "patch apiservices.apiregistration.k8s.io/"+APIServiceName, "docs", DocsURL)
		} else {
			c.warn("patch", err, "cannot patch the APIService caBundle; retrying", "docs", DocsURL)
		}
		return err
	}
	log.Info("Set the APIService caBundle to the serving CA and enabled TLS verification", "apiService", APIServiceName)
	return nil
}

// warn logs at most once per warnInterval for each kind of problem.
func (c *Controller) warn(kind string, err error, msg string, keysAndValues ...any) {
	c.warnMu.Lock()
	now := c.now()
	last, seen := c.lastWarn[kind]
	if seen && now.Sub(last) < warnInterval {
		c.warnMu.Unlock()
		return
	}
	c.lastWarn[kind] = now
	c.warnMu.Unlock()
	if err != nil {
		log.Error(err, msg, keysAndValues...)
		return
	}
	log.Info(msg, keysAndValues...)
}
