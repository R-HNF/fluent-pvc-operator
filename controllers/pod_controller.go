package controllers

import (
	"context"
	"fmt"

	"golang.org/x/xerrors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	fluentpvcv1alpha1 "github.com/st-tech/fluent-pvc-operator/api/v1alpha1"
	"github.com/st-tech/fluent-pvc-operator/constants"
)

//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcs,verbs=get;list;watch
//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcs/status,verbs=get
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

type podReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewPodReconciler(mgr ctrl.Manager) *podReconciler {
	return &podReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
}

func (r *podReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("podReconciler").WithName("Reconcile")

	// FluentPVCBinding に定義された Pod を監視する。
	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}

	// annotation で fluent-pvc-operator.tech.zozo.com/fluent-pvc-name: hogehoge が定義されている Pod を処理対象とする
	var fluentPVCName string
	if v, ok := pod.Labels[constants.PodLabelFluentPVCName]; !ok {
		// not target
		return ctrl.Result{}, nil
	} else {
		fluentPVCName = v
	}

	// Pod の phase が Running である場合のみ処理対象とする
	if !isPodRunningPhase(pod) {
		logger.Info(fmt.Sprintf("Skip processing because pod='%s' is '%s' phase.", pod.Name, pod.Status.Phase))
		return ctrl.Result{}, nil
	}

	// fluent-pvc-operator.tech.zozo.com/fluent-pvc-name の value で指定された FluentPVC が定義されていない場合エラーとする
	fpvc := &fluentpvcv1alpha1.FluentPVC{}
	if err := r.Get(ctx, client.ObjectKey{Name: fluentPVCName}, fpvc); err != nil {
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}

	// Unhealthy Pod Auto Deletion: Injected Sidecar Container の異常を検知して Pod を自動で削除します
	// Monitor the Sidecar Container status.
	// Delete the Pod when the Sidecar Container is terminated with exit code != 0.
	// Sidecar Container が異常終了したら Pod を削除する。
	// FluentPVC の設定で Sidecar Container の Health Check が Enable である場合のみ処理対象とする
	if !fpvc.Spec.DeletePodIfSidecarContainerTerminationDetected {
		logger.Info(fmt.Sprintf(
			"Skip processing because deletePodIfSidecarContainerTerminationDetected=false in fluentpvc='%s' for pod='%s'",
			fpvc.Name, pod.Name,
		))
		return ctrl.Result{}, nil
	}

	// Sidecar Container の状態を確認し、 Terminated with exit code != 0 である場合 Pod を削除する

	containerName := fpvc.Spec.SidecarContainerTemplate.Name
	status := findContainerStatusByName(&pod.Status, containerName)
	if status == nil {
		return ctrl.Result{}, xerrors.New(fmt.Sprintf("Container='%s' does not have any status.", containerName))
	}
	if status.RestartCount == 0 && status.State.Terminated == nil {
		logger.Info(fmt.Sprintf(
			"Container='%s' in the pod='%s' has never been terminated.",
			containerName, pod.Name,
		))
		return ctrl.Result{}, nil
	}

	logger.Info(fmt.Sprintf(
		"Delete the pod='%s' in the background because the container='%s' termination is detected.",
		pod.Name, containerName,
	))

	// TODO: Respects PodDisruptionBudget.
	// (todo) Pod 削除時に PDB の設定や Deployment の場合は minAvailable を考慮する
	// (todo) Sidecar Container が Not Ready となった場合も削除対象とする
	deleteOptions := deleteOptionsBackground(&pod.UID, &pod.ResourceVersion)
	if err := r.Delete(ctx, pod, deleteOptions); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *podReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	// TODO: Monitor at shorter intervals.

	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(pred).
		For(&corev1.Pod{}).
		Complete(r)
}
