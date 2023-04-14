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
	"os"
	"strconv"
	"time"

	accountv1 "github.com/labring/sealos/controllers/account/api/v1"

	"k8s.io/client-go/util/retry"

	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	meteringcommonv1 "github.com/labring/sealos/controllers/common/metering/api/v1"
	meteringv1 "github.com/labring/sealos/controllers/metering/api/v1"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MeteringReconcile reconciles a Metering object
type MeteringReconcile struct {
	client.Client
	Scheme *runtime.Scheme
	logr.Logger
	MeteringSystemNamespace string
	MeteringInterval        int
}

//+kubebuilder:rbac:groups=metering.sealos.io,resources=meterings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metering.sealos.io,resources=meterings/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=metering.sealos.io,resources=meterings/finalizers,verbs=update
//+kubebuilder:rbac:groups=metering.common.sealos.io,resources=extensionresourceprices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metering.common.sealos.io,resources=resources,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metering.common.sealos.io,resources=resources/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=account.sealos.io,resources=accountbalances,verbs=get;list;watch;create;update;patch;delete

// Reconcile metering belong to account，resourceQuota belong to metering
func (r *MeteringReconcile) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Logger.Info("enter reconcile", "name: ", req.Name, "namespace: ", req.Namespace)
	// when user ns create enter this logic
	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err == nil {
		// if create a user namespace ,will enter this reconcile
		if _, ok := ns.Annotations[meteringv1.UserAnnotationOwnerKey]; ok {
			if ns.DeletionTimestamp != nil {
				r.Logger.Info("namespace is deleting,want to delete metering", "namespace", ns.Name)
				err := r.DelMetering(ctx, meteringv1.MeteringPrefix+ns.Name, r.MeteringSystemNamespace)
				return ctrl.Result{}, err
			}
			if err := r.initMetering(ctx, ns); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	} else if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	// when Resource create will enter this logic
	resources := &meteringcommonv1.Resource{}
	if err := r.Get(ctx, req.NamespacedName, resources); err == nil {
		if resources.Status.Status == meteringcommonv1.Complete || !resources.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
		r.Logger.Info("enter update resource used", "resource name: ", req.Name, "resource namespace: ", req.Namespace, "resource Used", resources.Spec.Resources)
		var metering meteringv1.Metering
		for resourceName, resourceInfo := range resources.Spec.Resources {
			if err := r.Get(ctx, types.NamespacedName{Namespace: r.MeteringSystemNamespace, Name: meteringv1.MeteringPrefix + resourceInfo.Namespace}, &metering); err != nil {
				if err := r.Get(ctx, types.NamespacedName{Namespace: r.MeteringSystemNamespace, Name: meteringv1.MeteringPrefix + req.Namespace}, &metering); err != nil {
					r.Logger.Error(err, "get metering failed", "name", meteringv1.MeteringPrefix+resourceInfo.Namespace)
					continue
				}
			}

			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &metering, func() error {
				if metering.Spec.Resources == nil {
					metering.Spec.Resources = make(map[corev1.ResourceName]meteringcommonv1.ResourceInfo)
				}
				if _, ok := metering.Spec.Resources[resourceName]; !ok {
					metering.Spec.Resources[resourceName] = resourceInfo
				} else {
					metering.Spec.Resources[resourceName] = meteringcommonv1.ResourceInfo{
						Timestamp: time.Now().Unix(),
						Namespace: metering.Spec.Resources[resourceName].Namespace,
						Cost:      metering.Spec.Resources[resourceName].Cost + resourceInfo.Cost,
						Used:      metering.Spec.Resources[resourceName].Used,
					}
					metering.Spec.Resources[resourceName].Used.Add(*resourceInfo.Used)
				}
				return nil
			}); err != nil {
				r.Logger.Error(err, "update metering failed", "name", meteringv1.MeteringPrefix+resourceInfo.Namespace)
				return ctrl.Result{Requeue: true}, err
			}
			resources.Status.Status = meteringcommonv1.Complete
			if err := r.Status().Update(ctx, resources); err != nil {
				r.Logger.Error(err, err.Error())
				return ctrl.Result{Requeue: true}, err
			}
		}
	} else if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	var metering meteringv1.Metering
	if err := r.Get(ctx, req.NamespacedName, &metering); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if overTimeInterval(metering) {
		r.Logger.Info("enter update metering", "metering name:", req.Name, "metering namespace:", req.Namespace, "lastUpdate Time", metering.Status.LatestUpdateTime, "now", time.Now().Unix(), "diff", time.Now().Unix()-metering.Status.LatestUpdateTime, "interval", int64(metering.Spec.TimeInterval))
		totalAmount, err := r.CalculateCost(metering)
		if err != nil {
			r.Logger.Error(err, err.Error())
			return ctrl.Result{}, err
		}
		if err := r.createAccountBalance(ctx, metering, totalAmount); err != nil {
			r.Logger.Error(err, err.Error())
			return ctrl.Result{}, fmt.Errorf("meteringName:%v,amount:%v,err:%v", metering.Name, totalAmount, err)
		}
		if err := r.clearResourceUsed(ctx, &metering); err != nil {
			r.Logger.Error(err, err.Error())
			return ctrl.Result{Requeue: true}, err
		}

		if err := r.updateBillingList(ctx, totalAmount, &metering); err != nil {
			r.Logger.Error(err, err.Error())
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{Requeue: true, RequeueAfter: time.Minute * time.Duration(r.MeteringInterval)}, nil
}

func overTimeInterval(metering meteringv1.Metering) bool {
	return time.Now().Unix()-metering.Status.LatestUpdateTime >= int64(time.Minute.Seconds())*int64(metering.Spec.TimeInterval)
}

func (r *MeteringReconcile) CalculateCost(metering meteringv1.Metering) (int64, error) {
	var totalAmount int64
	for _, resourceValue := range metering.Spec.Resources {
		totalAmount += resourceValue.Cost
	}
	return totalAmount, nil
}

func (r *MeteringReconcile) clearResourceUsed(ctx context.Context, metering *meteringv1.Metering) error {
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, metering, func() error {
		for k := range metering.Spec.Resources {
			delete(metering.Spec.Resources, k)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *MeteringReconcile) updateBillingList(ctx context.Context, amount int64, metering *meteringv1.Metering) error {
	//r.Logger.Info("enter metering updateBillingList", "metering name: ", metering.Name, "metering namespace: ", metering.Namespace, "lastupdat Time", metering.Status.LatestUpdateTime)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, types.NamespacedName{Name: metering.Name, Namespace: metering.Namespace}, metering); err != nil {
			return err
		}
		metering.Status.BillingListH = append(metering.Status.BillingListH, NewBillingList(meteringv1.HOUR, amount))
		if len(metering.Status.BillingListH) >= 24 {
			var totalAmountD int64
			for i := 0; i < 24; i++ {
				totalAmountD += metering.Status.BillingListH[i].Amount
			}
			metering.Status.BillingListH = metering.Status.BillingListH[24:]
			metering.Status.BillingListD = append(metering.Status.BillingListD, NewBillingList(meteringv1.DAY, totalAmountD))
		}
		metering.Status.TotalAmount += amount
		metering.Status.LatestUpdateTime = time.Now().Unix()
		metering.Status.SeqID++
		return r.Status().Update(ctx, metering)
	})
}

func NewBillingList(TimeInterval meteringv1.TimeIntervalType, amount int64) meteringv1.BillingList {
	return meteringv1.BillingList{
		Timestamp:    time.Now().Unix(),
		TimeInterval: TimeInterval,
		Settled:      false,
		Amount:       amount,
	}
}

func (r *MeteringReconcile) initMetering(ctx context.Context, ns corev1.Namespace) error {
	// get or create resourceQuota
	if err := r.syncResourceQuota(ctx, ns.Name); err != nil {
		r.Error(err, "Failed to syncResourceQuota")
		return err
	}
	if err := r.syncMetering(ctx, ns); err != nil {
		r.Logger.Error(err, "Failed to create Metering", "nsName", ns.Name)
		return client.IgnoreNotFound(err)
	}
	return nil
}

func (r *MeteringReconcile) syncMetering(ctx context.Context, ns corev1.Namespace) error {
	metering := meteringv1.Metering{
		ObjectMeta: metav1.ObjectMeta{
			Name:      meteringv1.MeteringPrefix + ns.Name,
			Namespace: r.MeteringSystemNamespace,
		},
	}
	if err := r.Get(ctx, types.NamespacedName{Name: metering.Name, Namespace: metering.Namespace}, &metering); err == nil {
		r.Logger.Info(fmt.Sprintf("metering %v already exists", metering.Name))
		return nil
	}

	r.Logger.V(1).Info("metering env", "METERING_INTERVAL", r.MeteringSystemNamespace, "timeInterval: ", r.MeteringInterval)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &metering, func() error {
		metering.Spec.Namespace = ns.Name
		metering.Spec.Owner = ns.Annotations[meteringv1.UserAnnotationOwnerKey]
		metering.Spec.TimeInterval = r.MeteringInterval
		if metering.Spec.Resources == nil {
			metering.Spec.Resources = make(map[corev1.ResourceName]meteringcommonv1.ResourceInfo)
		}
		return nil
	}); err != nil {
		r.Error(err, "Failed to update Metering")
		return err
	}
	r.Logger.Info("metering Spec", "metering.Spec", metering.Spec)

	return nil
}

func (r *MeteringReconcile) syncResourceQuota(ctx context.Context, nsName string) error {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      meteringv1.ResourceQuotaPrefix + nsName,
			Namespace: nsName,
		},
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Spec.Hard = meteringv1.DefaultResourceQuota()
		return nil
	}); err != nil {
		return fmt.Errorf("sync resource quota failed: %v", err)
	}
	return nil
}

func (r *MeteringReconcile) DelMetering(ctx context.Context, name, namespace string) error {
	metering := meteringv1.Metering{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &metering)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	err = r.Delete(ctx, &metering)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

func (r *MeteringReconcile) createAccountBalance(ctx context.Context, metering meteringv1.Metering, totalAmount int64) error {
	if totalAmount == 0 {
		return nil
	} else if totalAmount < 0 {
		return fmt.Errorf("deduction amount is <0")
	}

	accountBalance := accountv1.AccountBalance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%v", accountv1.AccountBalancePrefix, metering.Spec.Owner, metering.Status.SeqID),
			Namespace: r.MeteringSystemNamespace,
		},
	}

	// only create accountBalance
	if err := r.Get(ctx, types.NamespacedName{Name: accountBalance.Name, Namespace: accountBalance.Namespace}, &accountBalance); err == nil {
		return fmt.Errorf("accountbalancnce have existed,name:%v", accountBalance.Name)
	} else if client.IgnoreNotFound(err) == nil {
		accountBalance.Spec.Owner = metering.Spec.Owner
		accountBalance.Spec.Timestamp = time.Now().Unix()
		accountBalance.Spec.Amount = totalAmount
		if metering.Spec.Resources != nil {
			for resourceName, resourceInfo := range metering.Spec.Resources {
				resourceInfo.ResourceName = string(resourceName)
				accountBalance.Spec.ResourceInfoList = append(accountBalance.Spec.ResourceInfoList, resourceInfo)
			}
		}
		if err := r.Create(ctx, &accountBalance); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	r.Logger.Info("create accountBalance", "name", accountBalance.Name, "accountBalance.spec", accountBalance.Spec, "metering.spec.Resource", metering.Spec.Resources)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MeteringReconcile) SetupWithManager(mgr ctrl.Manager) error {
	const controllerName = "metering-controller"
	r.Logger = ctrl.Log.WithName(controllerName)
	r.Logger.V(1).Info("reconcile controller metering")

	// get env METERING_SYSTEM_NAMESPACE and METERING_INTERVAL
	r.MeteringSystemNamespace = os.Getenv(meteringv1.METERINGNAMESPACEENV)
	if os.Getenv(meteringv1.METERINGNAMESPACEENV) == "" {
		r.MeteringSystemNamespace = meteringv1.DEFAULTMETERINGNAMESPACE
	}

	timeInterval := os.Getenv("METERING_INTERVAL")

	if timeInterval == "" || timeInterval == "0" {
		timeInterval = DefaultInterval()
	}
	r.MeteringInterval, _ = strconv.Atoi(timeInterval)
	if r.MeteringInterval == 0 {
		r.MeteringInterval = DefaultIntervalInt()
	}
	r.Logger.Info("metering env", meteringv1.METERINGNAMESPACEENV, r.MeteringSystemNamespace, "timeinterval:", r.MeteringInterval, "envtimeinterval", os.Getenv("METERING_INTERVAL"))
	return ctrl.NewControllerManagedBy(mgr).
		For(&meteringv1.Metering{}).
		Watches(&source.Kind{Type: &corev1.Namespace{}}, &handler.EnqueueRequestForObject{}).
		Watches(&source.Kind{Type: &meteringcommonv1.Resource{}}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

func DefaultInterval() string {
	return "60"
}
func DefaultIntervalInt() int {
	return 60
}