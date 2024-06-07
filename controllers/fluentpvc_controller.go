package controllers

import (
	"context"
	"fmt"

	"golang.org/x/xerrors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	fluentpvcv1alpha1 "github.com/st-tech/fluent-pvc-operator/api/v1alpha1"
	"github.com/st-tech/fluent-pvc-operator/constants"
)

//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcs/finalizers,verbs=update

// FluentPVC
// Provision する PVC の Template や Sidecar Container の定義など fluent-pvc-operator を利用するために必要な設定を定義する Custom Resource です。
// 詳細な設定方法は [Configuration] の節で書きます。

type fluentPVCReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewFluentPVCReconciler(mgr ctrl.Manager) *fluentPVCReconciler {
	return &fluentPVCReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
}

func (r *fluentPVCReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("fluentPVCReconciler").WithName("Reconcile")

	// 処理対象の FluentPVC を Owner Controller として持つ全ての FluentPVCBinding の Finalizer を監視する。
	fpvc := &fluentpvcv1alpha1.FluentPVC{}
	if err := r.Get(ctx, req.NamespacedName, fpvc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}

	// 処理対象の FluentPVC を Owner Controller とする全ての FluentPVCBinding の Finalizer が削除されていない場合、
	// 処理対象の FluentPVC に Finalizer fluent-pvc-operator.tech.zozo.com/fluentpvc-protection を付与する

	// 処理対象の FluentPVC を Owner Controller とする全ての FluentPVCBinding の Finalizer が削除されている場合、
	// 処理対象の FluentPVC から Finalizer fluent-pvc-operator.tech.zozo.com/fluentpvc-protection を削除する

	bindings := &fluentpvcv1alpha1.FluentPVCBindingList{}
	if err := r.List(ctx, bindings, matchingOwnerControllerField(fpvc.Name)); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}

	// 全ての FluentPVCBinding から Finalizer が削除されたら FluentPVC の Finalizer を削除する。
	allBindingsFinalized := true
	for _, b := range bindings.Items {
		if controllerutil.ContainsFinalizer(&b, constants.FluentPVCBindingFinalizerName) {
			allBindingsFinalized = false
			break
		}
	}
	if allBindingsFinalized {
		logger.Info(fmt.Sprintf(
			"Remove the finalizer: %s from fluentpvc: %s because all fluentpvcbindings are finalized.",
			constants.FluentPVCFinalizerName, fpvc.Name,
		))
		controllerutil.RemoveFinalizer(fpvc, constants.FluentPVCFinalizerName)
	} else {
		logger.Info(fmt.Sprintf(
			"Add the finalizer: %s to fluentpvc: %s because some fluentpvcbindings are not finalized.",
			constants.FluentPVCFinalizerName, fpvc.Name,
		))
		controllerutil.AddFinalizer(fpvc, constants.FluentPVCFinalizerName)
	}
	if err := r.Update(ctx, fpvc); err != nil {
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *fluentPVCReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&fluentpvcv1alpha1.FluentPVCBinding{},
		constants.OwnerControllerField,
		indexFluentPVCBindingByOwnerFluentPVC,
	); err != nil {
		return xerrors.Errorf("Unexpected error occurred.: %w", err)
	}
	pred := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&fluentpvcv1alpha1.FluentPVC{}).
		Owns(&fluentpvcv1alpha1.FluentPVCBinding{}).
		WithEventFilter(pred).
		Complete(r)
}
