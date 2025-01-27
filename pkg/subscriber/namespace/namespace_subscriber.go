// Copyright 2019 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package namespace

import (
	"reflect"

	"errors"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"

	chnv1alpha1 "github.com/IBM/multicloud-operators-channel/pkg/apis/app/v1alpha1"
	dplv1alpha1 "github.com/IBM/multicloud-operators-deployable/pkg/apis/app/v1alpha1"
	dplutils "github.com/IBM/multicloud-operators-deployable/pkg/utils"
	appv1alpha1 "github.com/IBM/multicloud-operators-subscription/pkg/apis/app/v1alpha1"
	kubesynchronizer "github.com/IBM/multicloud-operators-subscription/pkg/synchronizer/kubernetes"
)

// SubscriberItem - defines the unit of namespace subscription
type SubscriberItem struct {
	appv1alpha1.SubscriberItem
	cache         cache.Cache
	controller    controller.Controller
	clusterscoped bool
	stopch        chan struct{}
}

type itemmap map[types.NamespacedName]*SubscriberItem

// Subscriber - information to run namespace subscription
type Subscriber struct {
	itemmap
	// hub cluster
	config *rest.Config
	scheme *runtime.Scheme
	// endpoint cluster
	manager      manager.Manager
	synchronizer *kubesynchronizer.KubeSynchronizer
}

var defaultSubscriber *Subscriber

var (
	defaultSubscription = &appv1alpha1.Subscription{}
	defaultChannel      = &chnv1alpha1.Channel{}
	defaultitem         = &appv1alpha1.SubscriberItem{
		Subscription: defaultSubscription,
		Channel:      defaultChannel,
	}
)

// Add does nothing for namespace subscriber, it generates cache for each of the item
func Add(mgr manager.Manager, hubconfig *rest.Config, syncid *types.NamespacedName, syncinterval int) error {
	// No polling, use cache. Add default one for cluster namespace
	var err error

	klog.Info("Setting up default namespace subscriber on ", syncid)

	sync := kubesynchronizer.GetDefaultSynchronizer()
	if sync == nil {
		err = kubesynchronizer.Add(mgr, hubconfig, syncid, syncinterval)
		if err != nil {
			klog.Error("Failed to initialize synchronizer for default namespace channel with error:", err)
			return err
		}

		sync = kubesynchronizer.GetDefaultSynchronizer()
	}

	if err != nil {
		klog.Error("Failed to create synchronizer for subscriber with error:", err)
		return err
	}

	defaultSubscriber = CreateNamespaceSubsriber(hubconfig, mgr.GetScheme(), mgr, sync)
	if defaultSubscriber == nil {
		errmsg := "failed to create default namespace subscriber"

		return errors.New(errmsg)
	}

	if syncid.String() != "/" {
		defaultitem.Channel.Spec.PathName = syncid.Namespace
		err = defaultSubscriber.SubscribeNamespaceItem(defaultitem, true)

		if err != nil {
			klog.Error("Failed to initialize default channel to cluster namespace")
			return err
		}
	}

	return nil
}

// SubscribeNamespaceItem adds namespace subscribe item to subscriber
func (ns *Subscriber) SubscribeNamespaceItem(subitem *appv1alpha1.SubscriberItem, clusterScoped bool) error {
	var err error

	if ns.itemmap == nil {
		ns.itemmap = make(map[types.NamespacedName]*SubscriberItem)
	}

	itemkey := types.NamespacedName{Name: subitem.Subscription.Name, Namespace: subitem.Subscription.Namespace}

	nssubitem, ok := ns.itemmap[itemkey]

	if !ok {
		nssubitem = &SubscriberItem{}
		nssubitem.clusterscoped = clusterScoped
		nssubitem.cache, err = cache.New(ns.config, cache.Options{Scheme: ns.scheme, Namespace: subitem.Channel.Namespace})

		if err != nil {
			klog.Error("Failed to create cache for Namespace subscriber item with error: ", err)
			return err
		}

		hubclient, err := client.New(ns.config, client.Options{})

		if err != nil {
			klog.Error("Failed to create client for Namespace subscriber item with error: ", err)
			return err
		}

		reconciler := &DeployableReconciler{
			Client:     hubclient,
			subscriber: ns,
			itemkey:    itemkey,
		}
		nssubitem.controller, err = controller.New("sub"+itemkey.String(), ns.manager, controller.Options{Reconciler: reconciler})

		if err != nil {
			klog.Error("Failed to create controller for Namespace subscriber item with error: ", err)
			return err
		}

		ifm, err := nssubitem.cache.GetInformer(&dplv1alpha1.Deployable{})

		if err != nil {
			klog.Error("Failed to get informer from cache with error: ", err)
			return err
		}

		src := &source.Informer{Informer: ifm}

		err = nssubitem.controller.Watch(src, &handler.EnqueueRequestForObject{}, dplutils.DeployablePredicateFunc)

		if err != nil {
			klog.Error("Failed to watch deployable with error: ", err)
			return err
		}

		nssubitem.stopch = make(chan struct{})

		go func() {
			err := nssubitem.cache.Start(nssubitem.stopch)
			if err != nil {
				klog.Error("Failed to start cache for Namespace subscriber item with error: ", err)
			}
		}()

		go func() {
			err := nssubitem.controller.Start(nssubitem.stopch)
			if err != nil {
				klog.Error("Failed to start controller for Namespace subscriber item with error: ", err)
			}
		}()

		subitem.DeepCopyInto(&nssubitem.SubscriberItem)
		ns.itemmap[itemkey] = nssubitem
	} else if !reflect.DeepEqual(nssubitem.SubscriberItem, subitem) {
		subitem.DeepCopyInto(&nssubitem.SubscriberItem)
		ns.itemmap[itemkey] = nssubitem
	}

	return nil
}

// SubscribeItem subscribes a subscriber item with namespace channel
func (ns *Subscriber) SubscribeItem(subitem *appv1alpha1.SubscriberItem) error {
	return ns.SubscribeNamespaceItem(subitem, false)
}

// UnsubscribeItem unsubscribes a namespace subscriber item
func (ns *Subscriber) UnsubscribeItem(key types.NamespacedName) error {
	nssubitem, ok := ns.itemmap[key]

	if ok {
		close(nssubitem.stopch)
	}

	ns.synchronizer.CleanupByHost(key, "subscription-"+key.String())

	delete(ns.itemmap, key)

	return nil
}

// GetDefaultSubscriber - returns the defajlt namespace subscriber
func GetDefaultSubscriber() appv1alpha1.Subscriber {
	if defaultSubscriber == nil {
		return nil
	}

	return defaultSubscriber
}

// CreateNamespaceSubsriber - create namespace subscriber with config to hub cluster, scheme of hub cluster and a syncrhonizer to local cluster
func CreateNamespaceSubsriber(config *rest.Config, scheme *runtime.Scheme, mgr manager.Manager, kubesync *kubesynchronizer.KubeSynchronizer) *Subscriber {
	if config == nil || kubesync == nil {
		klog.Error("Can not create namespace subscriber with config: ", config, " kubenetes synchronizer: ", kubesync)
		return nil
	}

	nssubscriber := &Subscriber{
		config:       config,
		scheme:       scheme,
		manager:      mgr,
		synchronizer: kubesync,
	}

	nssubscriber.itemmap = make(map[types.NamespacedName]*SubscriberItem)

	return nssubscriber
}
