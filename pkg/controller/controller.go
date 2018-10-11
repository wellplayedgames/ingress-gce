/*
Copyright 2015 The Kubernetes Authors.

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

package controller

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	unversionedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	backendconfigv1beta1 "k8s.io/ingress-gce/pkg/apis/backendconfig/v1beta1"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/instances"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/gce/cloud/meta"

	"k8s.io/ingress-gce/pkg/annotations"
	"k8s.io/ingress-gce/pkg/context"
	"k8s.io/ingress-gce/pkg/controller/translator"
	"k8s.io/ingress-gce/pkg/loadbalancers"
	ingsync "k8s.io/ingress-gce/pkg/sync"
	"k8s.io/ingress-gce/pkg/tls"
	"k8s.io/ingress-gce/pkg/utils"
)

// LoadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer, via loadBalancerConfig.
type LoadBalancerController struct {
	ctx *context.ControllerContext

	ingLister  utils.StoreToIngressLister
	nodeLister cache.Indexer
	nodes      *NodeController

	// TODO: Watch secrets
	ingQueue   utils.TaskQueue
	Translator *translator.Translator
	stopCh     chan struct{}
	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex
	shutdown bool
	// tlsLoader loads secrets from the Kubernetes apiserver for Ingresses.
	tlsLoader tls.TlsLoader
	// hasSynced returns true if all associated sub-controllers have synced.
	// Abstracted into a func for testing.
	hasSynced func() bool

	// Resource pools.
	instancePool instances.NodePool
	l7Pool       loadbalancers.LoadBalancerPool

	// syncer implementation for backends
	backendSyncer backends.Syncer
	// linker implementations for backends
	negLinker backends.Linker
	igLinker  backends.Linker

	// Ingress sync + GC implementation
	ingSyncer ingsync.Syncer
}

// NewLoadBalancerController creates a controller for gce loadbalancers.
func NewLoadBalancerController(
	ctx *context.ControllerContext,
	stopCh chan struct{}) *LoadBalancerController {

	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(glog.Infof)
	broadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{
		Interface: ctx.KubeClient.Core().Events(""),
	})
	healthChecker := healthchecks.NewHealthChecker(ctx.Cloud, ctx.HealthCheckPath, ctx.DefaultBackendHealthCheckPath, ctx.ClusterNamer, ctx.DefaultBackendSvcPortID.Service)
	instancePool := instances.NewNodePool(ctx.Cloud, ctx.ClusterNamer)
	backendPool := backends.NewPool(ctx.Cloud, ctx.ClusterNamer, true)
	lbc := LoadBalancerController{
		ctx:           ctx,
		ingLister:     utils.StoreToIngressLister{Store: ctx.IngressInformer.GetStore()},
		nodeLister:    ctx.NodeInformer.GetIndexer(),
		Translator:    translator.NewTranslator(ctx),
		tlsLoader:     &tls.TLSCertsFromSecretsLoader{Client: ctx.KubeClient},
		stopCh:        stopCh,
		hasSynced:     ctx.HasSynced,
		nodes:         NewNodeController(ctx, instancePool),
		instancePool:  instancePool,
		l7Pool:        loadbalancers.NewLoadBalancerPool(ctx.Cloud, ctx.ClusterNamer),
		backendSyncer: backends.NewBackendSyncer(backendPool, healthChecker, ctx.ClusterNamer, ctx.BackendConfigEnabled),
		negLinker:     backends.NewNEGLinker(backendPool, ctx.Cloud, ctx.ClusterNamer),
		igLinker:      backends.NewInstanceGroupLinker(instancePool, backendPool, ctx.ClusterNamer),
	}
	lbc.ingSyncer = ingsync.NewIngressSyncer(&lbc)

	lbc.ingQueue = utils.NewPeriodicTaskQueue("ingresses", lbc.sync)

	// Ingress event handlers.
	ctx.IngressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng := obj.(*extensions.Ingress)
			if !utils.IsGLBCIngress(addIng) {
				glog.V(4).Infof("Ignoring add for ingress %v based on annotation %v", utils.IngressKeyFunc(addIng), annotations.IngressClassKey)
				return
			}

			glog.V(3).Infof("Ingress %v added, enqueuing", utils.IngressKeyFunc(addIng))
			lbc.ctx.Recorder(addIng.Namespace).Eventf(addIng, apiv1.EventTypeNormal, "ADD", utils.IngressKeyFunc(addIng))
			lbc.ingQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			delIng := obj.(*extensions.Ingress)
			if !utils.IsGLBCIngress(delIng) {
				glog.V(4).Infof("Ignoring delete for ingress %v based on annotation %v", utils.IngressKeyFunc(delIng), annotations.IngressClassKey)
				return
			}

			glog.V(3).Infof("Ingress %v deleted, enqueueing", utils.IngressKeyFunc(delIng))
			lbc.ingQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			curIng := cur.(*extensions.Ingress)
			if !utils.IsGLBCIngress(curIng) {
				return
			}
			if reflect.DeepEqual(old, cur) {
				glog.V(3).Infof("Periodic enqueueing of %v", utils.IngressKeyFunc(curIng))
			} else {
				glog.V(3).Infof("Ingress %v changed, enqueuing", utils.IngressKeyFunc(curIng))
			}

			lbc.ingQueue.Enqueue(cur)
		},
	})

	// Service event handlers.
	ctx.ServiceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*apiv1.Service)
			ings := lbc.ctx.IngressesForService(svc)
			lbc.ingQueue.Enqueue(convert(ings)...)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				svc := cur.(*apiv1.Service)
				ings := lbc.ctx.IngressesForService(svc)
				lbc.ingQueue.Enqueue(convert(ings)...)
			}
		},
		// Ingress deletes matter, service deletes don't.
	})

	// BackendConfig event handlers.
	if ctx.BackendConfigEnabled {
		ctx.BackendConfigInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				beConfig := obj.(*backendconfigv1beta1.BackendConfig)
				ings := lbc.ctx.IngressesForBackendConfig(beConfig)
				lbc.ingQueue.Enqueue(convert(ings)...)

			},
			UpdateFunc: func(old, cur interface{}) {
				if !reflect.DeepEqual(old, cur) {
					beConfig := cur.(*backendconfigv1beta1.BackendConfig)
					ings := lbc.ctx.IngressesForBackendConfig(beConfig)
					lbc.ingQueue.Enqueue(convert(ings)...)
				}
			},
			DeleteFunc: func(obj interface{}) {
				beConfig := obj.(*backendconfigv1beta1.BackendConfig)
				ings := lbc.ctx.IngressesForBackendConfig(beConfig)
				lbc.ingQueue.Enqueue(convert(ings)...)
			},
		})
	}

	// Register health check on controller context.
	ctx.AddHealthCheck("ingress", func() error {
		_, err := backendPool.Get("foo", meta.VersionGA)

		// If this container is scheduled on a node without compute/rw it is
		// effectively useless, but it is healthy. Reporting it as unhealthy
		// will lead to container crashlooping.
		if utils.IsHTTPErrorCode(err, http.StatusForbidden) {
			glog.Infof("Reporting cluster as healthy, but unable to list backends: %v", err)
			return nil
		}
		return utils.IgnoreHTTPNotFound(err)
	})

	glog.V(3).Infof("Created new loadbalancer controller")

	return &lbc
}

func (lbc *LoadBalancerController) Init() {
	// TODO(rramkumar): Try to get rid of this "Init".
	lbc.instancePool.Init(lbc.Translator)
	lbc.backendSyncer.Init(lbc.Translator)
}

// Run starts the loadbalancer controller.
func (lbc *LoadBalancerController) Run() {
	glog.Infof("Starting loadbalancer controller")
	go lbc.ingQueue.Run(time.Second, lbc.stopCh)
	lbc.nodes.Run(lbc.stopCh)

	<-lbc.stopCh
	glog.Infof("Shutting down Loadbalancer Controller")
}

// Stop stops the loadbalancer controller. It also deletes cluster resources
// if deleteAll is true.
func (lbc *LoadBalancerController) Stop(deleteAll bool) error {
	// Stop is invoked from the http endpoint.
	lbc.stopLock.Lock()
	defer lbc.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !lbc.shutdown {
		close(lbc.stopCh)
		glog.Infof("Shutting down controller queues.")
		lbc.ingQueue.Shutdown()
		lbc.nodes.Shutdown()
		lbc.shutdown = true
	}

	// Deleting shared cluster resources is idempotent.
	// TODO(rramkumar): Do we need deleteAll? Can we get rid of its' flag?
	if deleteAll {
		glog.Infof("Shutting down cluster manager.")
		if err := lbc.l7Pool.Shutdown(); err != nil {
			return err
		}
		// The backend pool will also delete instance groups.
		return lbc.backendSyncer.Shutdown()
	}
	return nil
}

// SyncBackends implements Controller.
func (lbc *LoadBalancerController) SyncBackends(state interface{}) error {
	// We expect state to be a syncState
	syncState, ok := state.(*syncState)
	if !ok {
		return fmt.Errorf("expected state type to be syncState, type was %T", state)
	}
	ingSvcPorts := syncState.urlMap.AllServicePorts()

	// Create instance groups and set named ports.
	igs, err := lbc.instancePool.EnsureInstanceGroupsAndPorts(lbc.ctx.ClusterNamer.InstanceGroup(), nodePorts(ingSvcPorts))
	if err != nil {
		return err
	}

	nodeNames, err := utils.GetReadyNodeNames(listers.NewNodeLister(lbc.nodeLister))
	if err != nil {
		return err
	}
	// Add/remove instances to the instance groups.
	if err = lbc.instancePool.Sync(nodeNames); err != nil {
		return err
	}

	// TODO: Remove this after deprecation
	ing := syncState.ing
	if utils.IsGCEMultiClusterIngress(syncState.ing) {
		// Add instance group names as annotation on the ingress and return.
		if ing.Annotations == nil {
			ing.Annotations = map[string]string{}
		}
		if err = setInstanceGroupsAnnotation(ing.Annotations, igs); err != nil {
			return err
		}
		if err = updateAnnotations(lbc.ctx.KubeClient, ing.Name, ing.Namespace, ing.Annotations); err != nil {
			return err
		}
		// This short-circuit will stop the syncer from moving to next step.
		return ingsync.ErrSkipBackendsSync
	}

	// Sync the backends
	if err := lbc.backendSyncer.Sync(ingSvcPorts); err != nil {
		return err
	}

	// Get the zones our groups live in.
	zones, err := lbc.Translator.ListZones()
	var groupKeys []backends.GroupKey
	for _, zone := range zones {
		groupKeys = append(groupKeys, backends.GroupKey{Zone: zone})
	}

	// Link backends to groups.
	for _, sp := range ingSvcPorts {
		var linkErr error
		if sp.NEGEnabled {
			// Link backend to NEG's if the backend has NEG enabled.
			linkErr = lbc.negLinker.Link(sp, groupKeys)
		} else if !sp.IsBucket() {
			// Otherwise, link backend to IG's.
			linkErr = lbc.igLinker.Link(sp, groupKeys)
		}
		if linkErr != nil {
			return linkErr
		}
	}

	return nil
}

// GCBackends implements Controller.
func (lbc *LoadBalancerController) GCBackends(state interface{}) error {
	// We expect state to be a gcState
	gcState, ok := state.(*gcState)
	if !ok {
		return fmt.Errorf("expected state type to be gcState, type was %T", state)
	}

	if err := lbc.backendSyncer.GC(gcState.svcPorts); err != nil {
		return err
	}

	// TODO(ingress#120): Move this to the backend pool so it mirrors creation
	if len(gcState.lbNames) == 0 {
		igName := lbc.ctx.ClusterNamer.InstanceGroup()
		glog.Infof("Deleting instance group %v", igName)
		if err := lbc.instancePool.DeleteInstanceGroup(igName); err != err {
			return err
		}
	}
	return nil
}

// SyncLoadBalancer implements Controller.
func (lbc *LoadBalancerController) SyncLoadBalancer(state interface{}) error {
	// We expect state to be a syncState
	syncState, ok := state.(*syncState)
	if !ok {
		return fmt.Errorf("expected state type to be syncState, type was %T", state)
	}

	lb, err := lbc.toRuntimeInfo(syncState.ing, syncState.urlMap)
	if err != nil {
		return err
	}

	// Create higher-level LB resources.
	if err := lbc.l7Pool.Sync(lb); err != nil {
		return err
	}
	return nil
}

// GCLoadBalancers implements Controller.
func (lbc *LoadBalancerController) GCLoadBalancers(state interface{}) error {
	// We expect state to be a gcState
	gcState, ok := state.(*gcState)
	if !ok {
		return fmt.Errorf("expected state type to be gcState, type was %T", state)
	}

	if err := lbc.l7Pool.GC(gcState.lbNames); err != nil {
		return err
	}

	return nil
}

// PostProcess implements Controller.
func (lbc *LoadBalancerController) PostProcess(state interface{}) error {
	// We expect state to be a syncState
	syncState, ok := state.(*syncState)
	if !ok {
		return fmt.Errorf("expected state type to be syncState, type was %T", state)
	}

	// Get the loadbalancer and update the ingress status.
	ing := syncState.ing
	k, err := utils.KeyFunc(ing)
	if err != nil {
		return fmt.Errorf("cannot get key for Ingress %s/%s: %v", ing.Namespace, ing.Name, err)
	}

	l7, err := lbc.l7Pool.Get(k)
	if err != nil {
		return fmt.Errorf("unable to get loadbalancer: %v", err)
	}
	if err := lbc.updateIngressStatus(l7, ing); err != nil {
		return fmt.Errorf("update ingress status error: %v", err)
	}
	return nil
}

// sync manages Ingress create/updates/deletes events from queue.
func (lbc *LoadBalancerController) sync(key string) error {
	if !lbc.hasSynced() {
		time.Sleep(context.StoreSyncPollPeriod)
		return fmt.Errorf("waiting for stores to sync")
	}
	glog.V(3).Infof("Syncing %v", key)

	// Create state needed for GC.
	gceIngresses, err := lbc.ingLister.ListGCEIngresses()
	if err != nil {
		return err
	}
	// gceSvcPorts contains the ServicePorts used by only single-cluster ingress.
	gceSvcPorts := lbc.ToSvcPorts(&gceIngresses)
	lbNames := lbc.ingLister.Store.ListKeys()
	gcState := &gcState{lbNames, gceSvcPorts}

	obj, ingExists, err := lbc.ingLister.Store.GetByKey(key)
	if !ingExists {
		glog.V(2).Infof("Ingress %q no longer exists, triggering GC", key)
		// GC will find GCE resources that were used for this ingress and delete them.
		return lbc.ingSyncer.GC(gcState)
	}

	// Get ingress and DeepCopy for assurance that we don't pollute other goroutines with changes.
	ing, ok := obj.(*extensions.Ingress)
	if !ok {
		return fmt.Errorf("invalid object (not of type Ingress), type was %T", obj)
	}
	ing = ing.DeepCopy()

	// Bootstrap state for GCP sync.
	urlMap, errs := lbc.Translator.TranslateIngress(ing, lbc.ctx.DefaultBackendSvcPortID)
	syncState := &syncState{urlMap, ing}
	if errs != nil {
		return fmt.Errorf("error while evaluating the ingress spec: %v", utils.JoinErrs(errs))
	}

	// Sync GCP resources.
	syncErr := lbc.ingSyncer.Sync(syncState)
	if syncErr != nil {
		lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "Sync", fmt.Sprintf("Error during sync: %v", syncErr.Error()))
	}

	// Garbage collection will occur regardless of an error occurring. If an error occurred,
	// it could have been caused by quota issues; therefore, garbage collecting now may
	// free up enough quota for the next sync to pass.
	if gcErr := lbc.ingSyncer.GC(gcState); gcErr != nil {
		return fmt.Errorf("error during sync %v, error during GC %v", syncErr, gcErr)
	}

	return syncErr
}

// updateIngressStatus updates the IP and annotations of a loadbalancer.
// The annotations are parsed by kubectl describe.
func (lbc *LoadBalancerController) updateIngressStatus(l7 *loadbalancers.L7, ing *extensions.Ingress) error {
	ingClient := lbc.ctx.KubeClient.Extensions().Ingresses(ing.Namespace)

	// Update IP through update/status endpoint
	ip := l7.GetIP()
	currIng, err := ingClient.Get(ing.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	currIng.Status = extensions.IngressStatus{
		LoadBalancer: apiv1.LoadBalancerStatus{
			Ingress: []apiv1.LoadBalancerIngress{
				{IP: ip},
			},
		},
	}
	if ip != "" {
		lbIPs := ing.Status.LoadBalancer.Ingress
		if len(lbIPs) == 0 || lbIPs[0].IP != ip {
			// TODO: If this update fails it's probably resource version related,
			// which means it's advantageous to retry right away vs requeuing.
			glog.Infof("Updating loadbalancer %v/%v with IP %v", ing.Namespace, ing.Name, ip)
			if _, err := ingClient.UpdateStatus(currIng); err != nil {
				return err
			}
			lbc.ctx.Recorder(ing.Namespace).Eventf(currIng, apiv1.EventTypeNormal, "CREATE", "ip: %v", ip)
		}
	}
	annotations, err := loadbalancers.GetLBAnnotations(l7, currIng.Annotations, lbc.backendSyncer)
	if err != nil {
		return err
	}

	if err := updateAnnotations(lbc.ctx.KubeClient, ing.Name, ing.Namespace, annotations); err != nil {
		return err
	}
	return nil
}

// toRuntimeInfo returns L7RuntimeInfo for the given ingress.
func (lbc *LoadBalancerController) toRuntimeInfo(ing *extensions.Ingress, urlMap *utils.GCEURLMap) (*loadbalancers.L7RuntimeInfo, error) {
	k, err := utils.KeyFunc(ing)
	if err != nil {
		return nil, fmt.Errorf("cannot get key for Ingress %v/%v: %v", ing.Namespace, ing.Name, err)
	}

	var tls []*loadbalancers.TLSCerts

	annotations := annotations.FromIngress(ing)
	// Load the TLS cert from the API Spec if it is not specified in the annotation.
	// TODO: enforce this with validation.
	if annotations.UseNamedTLS() == "" {
		tls, err = lbc.tlsLoader.Load(ing)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// TODO: this path should be removed when external certificate managers migrate to a better solution.
				const msg = "Could not find TLS certificates. Continuing setup for the load balancer to serve HTTP. Note: this behavior is deprecated and will be removed in a future version of ingress-gce"
				lbc.ctx.Recorder(ing.Namespace).Eventf(ing, apiv1.EventTypeWarning, "Sync", msg)
			} else {
				glog.Errorf("Could not get certificates for ingress %s/%s: %v", ing.Namespace, ing.Name, err)
				return nil, err
			}
		}
	}

	return &loadbalancers.L7RuntimeInfo{
		Name:         k,
		TLS:          tls,
		TLSName:      annotations.UseNamedTLS(),
		AllowHTTP:    annotations.AllowHTTP(),
		StaticIPName: annotations.StaticIPName(),
		UrlMap:       urlMap,
	}, nil
}

func updateAnnotations(client kubernetes.Interface, name, namespace string, annotations map[string]string) error {
	ingClient := client.Extensions().Ingresses(namespace)
	currIng, err := ingClient.Get(name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currIng.Annotations, annotations) {
		glog.V(3).Infof("Updating annotations of %v/%v", namespace, name)
		currIng.Annotations = annotations
		if _, err := ingClient.Update(currIng); err != nil {
			return err
		}
	}
	return nil
}

// ToSvcPorts is a helper method over translator.TranslateIngress to process a list of ingresses.
// Note: This method is used for GC.
func (lbc *LoadBalancerController) ToSvcPorts(ings *extensions.IngressList) []utils.ServicePort {
	var knownPorts []utils.ServicePort
	for _, ing := range ings.Items {
		urlMap, _ := lbc.Translator.TranslateIngress(&ing, lbc.ctx.DefaultBackendSvcPortID)
		knownPorts = append(knownPorts, urlMap.AllServicePorts()...)
	}
	return knownPorts
}
