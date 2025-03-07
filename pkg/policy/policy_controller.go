package policy

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	kyvernov1beta1 "github.com/kyverno/kyverno/api/kyverno/v1beta1"
	backgroundcommon "github.com/kyverno/kyverno/pkg/background/common"
	"github.com/kyverno/kyverno/pkg/client/clientset/versioned"
	"github.com/kyverno/kyverno/pkg/client/clientset/versioned/scheme"
	kyvernov1informers "github.com/kyverno/kyverno/pkg/client/informers/externalversions/kyverno/v1"
	kyvernov1beta1informers "github.com/kyverno/kyverno/pkg/client/informers/externalversions/kyverno/v1beta1"
	kyvernov1listers "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v1"
	kyvernov1beta1listers "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v1beta1"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/config"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	"github.com/kyverno/kyverno/pkg/event"
	"github.com/kyverno/kyverno/pkg/metrics"
	datautils "github.com/kyverno/kyverno/pkg/utils/data"
	engineutils "github.com/kyverno/kyverno/pkg/utils/engine"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const (
	// maxRetries is the number of times a Policy will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a deployment is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15
)

// PolicyController is responsible for synchronizing Policy objects stored
// in the system with the corresponding policy violations
type PolicyController struct {
	client        dclient.Interface
	kyvernoClient versioned.Interface
	engine        engineapi.Engine

	pInformer  kyvernov1informers.ClusterPolicyInformer
	npInformer kyvernov1informers.PolicyInformer

	eventGen      event.Interface
	eventRecorder record.EventRecorder

	// Policies that need to be synced
	queue workqueue.RateLimitingInterface

	// pLister can list/get policy from the shared informer's store
	pLister kyvernov1listers.ClusterPolicyLister

	// npLister can list/get namespace policy from the shared informer's store
	npLister kyvernov1listers.PolicyLister

	// urLister can list/get update request from the shared informer's store
	urLister kyvernov1beta1listers.UpdateRequestLister

	// nsLister can list/get namespaces from the shared informer's store
	nsLister corev1listers.NamespaceLister

	informersSynced []cache.InformerSynced

	// helpers to validate against current loaded configuration
	configHandler config.Configuration

	reconcilePeriod time.Duration

	log logr.Logger

	metricsConfig metrics.MetricsConfigManager
}

// NewPolicyController create a new PolicyController
func NewPolicyController(
	kyvernoClient versioned.Interface,
	client dclient.Interface,
	engine engineapi.Engine,
	pInformer kyvernov1informers.ClusterPolicyInformer,
	npInformer kyvernov1informers.PolicyInformer,
	urInformer kyvernov1beta1informers.UpdateRequestInformer,
	configHandler config.Configuration,
	eventGen event.Interface,
	namespaces corev1informers.NamespaceInformer,
	log logr.Logger,
	reconcilePeriod time.Duration,
	metricsConfig metrics.MetricsConfigManager,
) (*PolicyController, error) {
	// Event broad caster
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.V(5).Info)
	eventInterface := client.GetEventsInterface()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: eventInterface})

	pc := PolicyController{
		client:          client,
		kyvernoClient:   kyvernoClient,
		engine:          engine,
		pInformer:       pInformer,
		npInformer:      npInformer,
		eventGen:        eventGen,
		eventRecorder:   eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "policy_controller"}),
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "policy"),
		configHandler:   configHandler,
		reconcilePeriod: reconcilePeriod,
		metricsConfig:   metricsConfig,
		log:             log,
	}

	pc.pLister = pInformer.Lister()
	pc.npLister = npInformer.Lister()
	pc.nsLister = namespaces.Lister()
	pc.urLister = urInformer.Lister()

	pc.informersSynced = []cache.InformerSynced{pInformer.Informer().HasSynced, npInformer.Informer().HasSynced, urInformer.Informer().HasSynced, namespaces.Informer().HasSynced}

	return &pc, nil
}

func (pc *PolicyController) canBackgroundProcess(p kyvernov1.PolicyInterface) bool {
	logger := pc.log.WithValues("policy", p.GetName())
	if !p.BackgroundProcessingEnabled() {
		if !p.GetSpec().HasGenerate() && !p.GetSpec().IsMutateExisting() {
			logger.V(4).Info("background processing is disabled")
			return false
		}
	}

	if err := ValidateVariables(p, true); err != nil {
		logger.V(4).Info("policy cannot be processed in the background")
		return false
	}

	return true
}

func (pc *PolicyController) addPolicy(obj interface{}) {
	logger := pc.log
	var p kyvernov1.PolicyInterface

	switch obj := obj.(type) {
	case *kyvernov1.ClusterPolicy:
		p = obj
	case *kyvernov1.Policy:
		p = obj
	default:
		return
	}

	logger.Info("policy created", "uid", p.GetUID(), "kind", p.GetKind(), "namespace", p.GetNamespace(), "name", p.GetName())

	if !pc.canBackgroundProcess(p) {
		return
	}

	logger.V(4).Info("queuing policy for background processing", "name", p.GetName())
	pc.enqueuePolicy(p)
}

func (pc *PolicyController) updatePolicy(old, cur interface{}) {
	logger := pc.log
	var oldP, curP kyvernov1.PolicyInterface

	switch obj := old.(type) {
	case *kyvernov1.ClusterPolicy:
		oldP = obj
	case *kyvernov1.Policy:
		oldP = obj
	default:
		return
	}

	switch obj := cur.(type) {
	case *kyvernov1.ClusterPolicy:
		curP = obj
	case *kyvernov1.Policy:
		curP = obj
	default:
		return
	}

	if !pc.canBackgroundProcess(curP) {
		return
	}

	if datautils.DeepEqual(oldP.GetSpec(), curP.GetSpec()) {
		return
	}

	logger.V(2).Info("updating policy", "name", oldP.GetName())
	if deleted, ok := ruleDeletion(oldP, curP); ok {
		err := pc.createURForDownstreamDeletion(deleted)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to create UR on rule deletion, clean up downstream resource may be failed: %v", err))
		}
	}

	pc.enqueuePolicy(curP)
}

func (pc *PolicyController) deletePolicy(obj interface{}) {
	logger := pc.log
	var p kyvernov1.PolicyInterface

	switch kubeutils.GetObjectWithTombstone(obj).(type) {
	case *kyvernov1.ClusterPolicy:
		p = kubeutils.GetObjectWithTombstone(obj).(*kyvernov1.ClusterPolicy)
	case *kyvernov1.Policy:
		p = kubeutils.GetObjectWithTombstone(obj).(*kyvernov1.Policy)
	default:
		logger.Info("Failed to get deleted object", "obj", obj)
		return
	}

	logger.Info("policy deleted", "uid", p.GetUID(), "kind", p.GetKind(), "namespace", p.GetNamespace(), "name", p.GetName())
	err := pc.createURForDownstreamDeletion(p)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to create UR on policy deletion, clean up downstream resource may be failed: %v", err))
	}
}

func (pc *PolicyController) enqueuePolicy(policy kyvernov1.PolicyInterface) {
	logger := pc.log
	key, err := cache.MetaNamespaceKeyFunc(policy)
	if err != nil {
		logger.Error(err, "failed to enqueue policy")
		return
	}
	pc.queue.Add(key)
}

// Run begins watching and syncing.
func (pc *PolicyController) Run(ctx context.Context, workers int) {
	logger := pc.log

	defer utilruntime.HandleCrash()
	defer pc.queue.ShutDown()

	logger.Info("starting")
	defer logger.Info("shutting down")

	if !cache.WaitForNamedCacheSync("PolicyController", ctx.Done(), pc.informersSynced...) {
		return
	}

	_, _ = pc.pInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    pc.addPolicy,
		UpdateFunc: pc.updatePolicy,
		DeleteFunc: pc.deletePolicy,
	})

	_, _ = pc.npInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    pc.addPolicy,
		UpdateFunc: pc.updatePolicy,
		DeleteFunc: pc.deletePolicy,
	})

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, pc.worker, time.Second)
	}

	go pc.forceReconciliation(ctx)

	<-ctx.Done()
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (pc *PolicyController) worker(ctx context.Context) {
	for pc.processNextWorkItem() {
	}
}

func (pc *PolicyController) processNextWorkItem() bool {
	key, quit := pc.queue.Get()
	if quit {
		return false
	}
	defer pc.queue.Done(key)
	err := pc.syncPolicy(key.(string))
	pc.handleErr(err, key)

	return true
}

func (pc *PolicyController) handleErr(err error, key interface{}) {
	logger := pc.log
	if err == nil {
		pc.queue.Forget(key)
		return
	}

	if pc.queue.NumRequeues(key) < maxRetries {
		logger.Error(err, "failed to sync policy", "key", key)
		pc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logger.V(2).Info("dropping policy out of queue", "key", key)
	pc.queue.Forget(key)
}

func (pc *PolicyController) syncPolicy(key string) error {
	logger := pc.log.WithName("syncPolicy")
	startTime := time.Now()
	logger.V(4).Info("started syncing policy", "key", key, "startTime", startTime)
	defer func() {
		logger.V(4).Info("finished syncing policy", "key", key, "processingTime", time.Since(startTime).String())
	}()

	policy, err := pc.getPolicy(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	} else {
		err = pc.handleMutate(key, policy)
		if err != nil {
			logger.Error(err, "failed to updateUR on mutate policy update")
		}

		err = pc.handleGenerate(key, policy)
		if err != nil {
			logger.Error(err, "failed to updateUR on generate policy update")
		}
	}
	return nil
}

func (pc *PolicyController) getPolicy(key string) (kyvernov1.PolicyInterface, error) {
	if ns, name, err := cache.SplitMetaNamespaceKey(key); err != nil {
		pc.log.Error(err, "failed to parse policy name", "policyName", key)
		return nil, err
	} else {
		isNamespacedPolicy := ns != ""
		if !isNamespacedPolicy {
			return pc.pLister.Get(name)
		}
		return pc.npLister.Policies(ns).Get(name)
	}
}

// forceReconciliation forces a background scan by adding all policies to the workqueue
func (pc *PolicyController) forceReconciliation(ctx context.Context) {
	logger := pc.log.WithName("forceReconciliation")
	ticker := time.NewTicker(pc.reconcilePeriod)

	for {
		select {
		case <-ticker.C:
			logger.Info("performing the background scan", "scan interval", pc.reconcilePeriod.String())
			pc.requeuePolicies()

		case <-ctx.Done():
			return
		}
	}
}

func (pc *PolicyController) requeuePolicies() {
	logger := pc.log.WithName("requeuePolicies")
	if cpols, err := pc.pLister.List(labels.Everything()); err == nil {
		for _, cpol := range cpols {
			if !pc.canBackgroundProcess(cpol) {
				continue
			}
			pc.enqueuePolicy(cpol)
		}
	} else {
		logger.Error(err, "unable to list ClusterPolicies")
	}
	if pols, err := pc.npLister.Policies(metav1.NamespaceAll).List(labels.Everything()); err == nil {
		for _, p := range pols {
			if !pc.canBackgroundProcess(p) {
				continue
			}
			pc.enqueuePolicy(p)
		}
	} else {
		logger.Error(err, "unable to list Policies")
	}
}

func (pc *PolicyController) handleUpdateRequest(ur *kyvernov1beta1.UpdateRequest, triggerResource *unstructured.Unstructured, rule kyvernov1.Rule, policy kyvernov1.PolicyInterface) (skip bool, err error) {
	namespaceLabels := engineutils.GetNamespaceSelectorsFromNamespaceLister(triggerResource.GetKind(), triggerResource.GetNamespace(), pc.nsLister, pc.log)
	policyContext, err := backgroundcommon.NewBackgroundContext(pc.client, ur, policy, triggerResource, pc.configHandler, namespaceLabels, pc.log)
	if err != nil {
		return false, fmt.Errorf("failed to build policy context for rule %s: %w", rule.Name, err)
	}

	engineResponse := pc.engine.ApplyBackgroundChecks(context.TODO(), policyContext)
	if len(engineResponse.PolicyResponse.Rules) == 0 {
		return true, nil
	}

	for _, ruleResponse := range engineResponse.PolicyResponse.Rules {
		if ruleResponse.Status() != engineapi.RuleStatusPass {
			pc.log.Error(err, "can not create new UR on policy update", "policy", policy.GetName(), "rule", rule.Name, "rule.Status", ruleResponse.Status())
			continue
		}

		pc.log.V(2).Info("creating new UR for generate")
		created, err := pc.kyvernoClient.KyvernoV1beta1().UpdateRequests(config.KyvernoNamespace()).Create(context.TODO(), ur, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}
		updated := created.DeepCopy()
		updated.Status.State = kyvernov1beta1.Pending
		_, err = pc.kyvernoClient.KyvernoV1beta1().UpdateRequests(config.KyvernoNamespace()).UpdateStatus(context.TODO(), updated, metav1.UpdateOptions{})
		if err != nil {
			return false, err
		}
	}
	return false, err
}

func generateTriggers(client dclient.Interface, rule kyvernov1.Rule, log logr.Logger) []*unstructured.Unstructured {
	list := &unstructured.UnstructuredList{}

	kinds := fetchUniqueKinds(rule)

	for _, kind := range kinds {
		mlist, err := client.ListResource(context.TODO(), "", kind, "", rule.MatchResources.Selector)
		if err != nil {
			log.Error(err, "failed to list matched resource")
			continue
		}
		list.Items = append(list.Items, mlist.Items...)
	}
	return convertlist(list.Items)
}
