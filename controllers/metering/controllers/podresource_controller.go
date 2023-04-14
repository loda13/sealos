/*
Copyright 2022.

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
	"fmt"
	"math"
	"os"
	"time"

	"github.com/labring/sealos/pkg/metering"

	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	meteringcommonv1 "github.com/labring/sealos/controllers/common/metering/api/v1"

	"github.com/go-logr/logr"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	meteringv1 "github.com/labring/sealos/controllers/metering/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ metering.ResourceControllerInterface = &PodResourceReconciler{}

// PodResourceReconciler reconciles a PodResource object
type PodResourceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	logr.Logger
	MeteringSystemNamespace string
}

//+kubebuilder:rbac:groups=metering.sealos.io,resources=podresources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metering.sealos.io,resources=podresources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=metering.sealos.io,resources=podresources/finalizers,verbs=update
//+kubebuilder:rbac:groups=metering.common.sealos.io,resources=resources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete

func (r *PodResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	podController := &meteringv1.PodResource{}
	err := r.Get(ctx, req.NamespacedName, podController)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	err = r.CreateOrUpdateExtensionResourcesPrice(ctx, podController)
	if err != nil {
		r.Logger.Error(err, "CreateOrUpdateExtensionResourcesPrice failed")
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, client.IgnoreNotFound(err)
	}
	// update resource used every podController.Spec.Interval Minutes
	if time.Now().Unix()-podController.Status.LatestUpdateTime >= int64(time.Minute.Minutes())*int64(podController.Spec.Interval) {
		if err := r.UpdateResourceUsed(ctx, podController); err != nil {
			r.Logger.Error(err, "UpdateResourceUsed failed")
			return ctrl.Result{Requeue: true}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{Requeue: true, RequeueAfter: time.Duration(podController.Spec.Interval) * time.Minute}, nil
}

// CreateOrUpdateExtensionResourcesPrice need to create a ExtensionResourcesPrice to make metering-quota know this resource
func (r *PodResourceReconciler) CreateOrUpdateExtensionResourcesPrice(ctx context.Context, obj client.Object) error {
	podController := obj.(*meteringv1.PodResource)
	extensionResourcesPrice := &meteringcommonv1.ExtensionResourcePrice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: r.MeteringSystemNamespace,
			Name:      meteringcommonv1.ExtensionResourcePricePrefix + meteringv1.PodResourcePricePrefix,
		},
	}
	r.Logger.Info("create or update extensionResourcePrice", "podController name", podController.Name, "podController Resources", podController.Spec.Resources)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, extensionResourcesPrice, func() error {
		extensionResourcesPrice.Spec.Resources = podController.Spec.Resources
		extensionResourcesPrice.Spec.ResourceName = podController.Spec.ResourceName
		extensionResourcesPrice.Spec.GroupVersionKinds = meteringv1.DefaultPodResourceGVK
		return nil
	}); err != nil {
		return fmt.Errorf("sync ExtensionResourcesPrice failed: %v", err)
	}
	r.Logger.V(1).Info("sync extensionResourcesPrice", "extensionResourcesPrice.Spec", extensionResourcesPrice.Spec)
	return nil
}

func (r *PodResourceReconciler) UpdateResourceUsed(ctx context.Context, obj client.Object) error {
	podController := obj.(*meteringv1.PodResource)
	var podList v1.PodList
	err := r.List(ctx, &podList)
	if err != nil {
		return err
	}
	for _, pod := range podList.Items {
		resourceInfos := make(map[v1.ResourceName]meteringcommonv1.ResourceInfo, 0)
		if !r.checkPodStatus(pod) || !r.checkPodNamespace(pod) {
			continue
		}
		for _, con := range pod.Spec.Containers {
			for resourceName := range podController.Spec.Resources {
				if resourceQuantity, ok := r.checkResourceExist(resourceName, con); ok {
					if _, ok := resourceInfos[resourceName]; !ok {
						resourceInfos[resourceName] = meteringcommonv1.ResourceInfo{
							Used:      &resourceQuantity,
							Timestamp: time.Now().Unix(),
							Namespace: pod.Namespace,
							Cost:      int64(float64(resourceQuantity.MilliValue()*podController.Spec.Resources[resourceName].Price) / float64(podController.Spec.Resources[resourceName].Unit.MilliValue())),
						}
					} else {
						resourceInfos[resourceName] = meteringcommonv1.ResourceInfo{
							Cost:      resourceInfos[resourceName].Cost + int64(float64(resourceQuantity.MilliValue()*podController.Spec.Resources[resourceName].Price)/float64(podController.Spec.Resources[resourceName].Unit.MilliValue())),
							Timestamp: time.Now().Unix(),
							Namespace: pod.Namespace,
							Used:      resourceInfos[resourceName].Used,
						}
						resourceInfos[resourceName].Used.Add(resourceQuantity)
					}
				}
			}
		}

		// storage resource not in pod container, so need to get it from resource quota
		resourceQuota := v1.ResourceQuota{}
		if err := r.Get(ctx, client.ObjectKey{Name: meteringv1.ResourceQuotaPrefix + pod.Namespace, Namespace: pod.Namespace}, &resourceQuota); err != nil {
			return err
		}
		//r.Logger.V(1).Info("resourceQuota", "resourceQuota", resourceQuota)
		storage := resourceQuota.Status.Used.Name("requests.storage", resource.BinarySI)
		resourceName := v1.ResourceName("storage")
		if _, ok := resourceInfos[resourceName]; !ok {
			resourceInfos[resourceName] = meteringcommonv1.ResourceInfo{
				Used:      storage,
				Timestamp: time.Now().Unix(),
				Namespace: pod.Namespace,
				Cost:      int64(math.Ceil(float64(storage.MilliValue()*podController.Spec.Resources[resourceName].Price) / float64(podController.Spec.Resources[resourceName].Unit.MilliValue()))),
			}
		} else {
			resourceInfos[resourceName] = meteringcommonv1.ResourceInfo{
				Cost:      resourceInfos[resourceName].Cost + int64(math.Ceil(float64(storage.MilliValue()*podController.Spec.Resources[resourceName].Price)/float64(podController.Spec.Resources[resourceName].Unit.MilliValue()))),
				Timestamp: time.Now().Unix(),
				Namespace: pod.Namespace,
				Used:      resourceInfos[resourceName].Used,
			}
			resourceInfos[resourceName].Used.Add(*storage)
		}
		if err := r.syncResource(ctx, pod, resourceInfos, *podController); err != nil {
			r.Logger.Error(err, "syncResource failed")
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Namespace: podController.Namespace, Name: podController.Name}, podController); err != nil {
			return client.IgnoreNotFound(err)
		}
		podController.Status.LatestUpdateTime = time.Now().Unix()
		podController.Status.SeqID++
		return r.Status().Update(ctx, podController)
	}); err != nil {
		r.Logger.Error(err, "UpdateResourceUsed failed")
		return fmt.Errorf("pod controller update err:%v", err)
	}
	r.Logger.Info("pod controller calculate resource success")
	return nil
}

func (r *PodResourceReconciler) checkPodStatus(pod v1.Pod) bool {
	if pod.Status.Phase == v1.PodRunning {
		return true
	}
	r.Logger.Info("pod status is  not ready", "pod name", pod.Name, "pod namespace", pod.Namespace, "pod status", pod.Status.Phase)
	return false
}

func (r *PodResourceReconciler) checkPodNamespace(pod v1.Pod) bool {
	ns := v1.Namespace{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: pod.Namespace}, &ns); err != nil {
		r.Logger.Error(err, "get namespace failed", "pod NS", pod.Namespace)
		return false
	}
	if _, ok := ns.Annotations[meteringv1.UserAnnotationOwnerKey]; !ok {
		r.Logger.Info("pod namespace is not user namespace", "pod name", pod.Name, "pod namespace", pod.Namespace)
		return false
	}
	return true
}

func (r *PodResourceReconciler) syncResource(ctx context.Context, pod v1.Pod, ResourceCR map[v1.ResourceName]meteringcommonv1.ResourceInfo, podController meteringv1.PodResource) error {
	Resource := meteringcommonv1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GetResourceName(pod.Namespace, pod.Name, podController.Status.SeqID),
			Namespace: r.MeteringSystemNamespace,
		},
	}

	if err := r.Get(ctx, types.NamespacedName{Namespace: Resource.Namespace, Name: Resource.Name}, &Resource); err == nil {
		return fmt.Errorf("resource already exist resource name:%v", Resource.Name)
	}
	Resource.Spec.Resources = ResourceCR
	r.Logger.Info("want to create resource", "resource name", GetResourceName(pod.Namespace, pod.Name, podController.Status.SeqID), "resource info", Resource.Spec.Resources)
	return r.Create(ctx, &Resource)
}

func (r *PodResourceReconciler) checkResourceExist(resourceName v1.ResourceName, container v1.Container) (resource.Quantity, bool) {
	//r.Logger.V(1).Info("pod container", "resourceName", resourceName, "container", container)
	if _, ok := container.Resources.Limits[resourceName]; ok {
		return container.Resources.Limits[resourceName], ok
		//r.Logger.Info("container.Resources.Limits resource not available", "resource name", resourceName)
	} else if _, ok := container.Resources.Requests[resourceName]; ok {
		return container.Resources.Requests[resourceName], ok
	}

	return resource.Quantity{}, false
}

func GetResourceName(namespaceName, podName string, seqID int64) string {
	return fmt.Sprintf("%s-%s-%s-%v", namespaceName, meteringv1.PodResourcePricePrefix, podName, seqID)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	const controllerName = "podResource-controller"
	r.Logger = ctrl.Log.WithName(controllerName)
	r.Logger.V(1).Info("reconcile podResource-controller")
	r.MeteringSystemNamespace = os.Getenv(meteringv1.METERINGNAMESPACEENV)
	if os.Getenv(meteringv1.METERINGNAMESPACEENV) == "" {
		r.MeteringSystemNamespace = meteringv1.DEFAULTMETERINGNAMESPACE
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&meteringv1.PodResource{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}))).
		Complete(r)
}