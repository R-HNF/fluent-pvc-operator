package controllers

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/xerrors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	fluentpvcv1alpha1 "github.com/st-tech/fluent-pvc-operator/api/v1alpha1"
	"github.com/st-tech/fluent-pvc-operator/constants"
	podutils "github.com/st-tech/fluent-pvc-operator/utils/pod"
)

//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcs,verbs=get;list;watch
//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcbindings,verbs=get;list;watch
//+kubebuilder:rbac:groups=fluent-pvc-operator.tech.zozo.com,resources=fluentpvcbindings/status,verbs=get
//+kubebuilder:rbac:groups="batch",resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;update

type pvcReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func NewPVCReconciler(mgr ctrl.Manager) *pvcReconciler {
	return &pvcReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}
}

func (r *pvcReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithName("pvcReconciler").WithName("Reconcile")

	// FluentPVCBinding に定義された PVC を監視する。
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, req.NamespacedName, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}

	// 処理対象の PVC と同名の FluentPVCBinding が存在していない場合、処理対象外とする（作りたてなど）
	b := &fluentpvcv1alpha1.FluentPVCBinding{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: pvc.Name}, b); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}

	// 同名の FluentPVCBinding が存在するのに、UID が異なる場合、処理対象外とする（何かおかしいがエラーにはしない。無駄にPVCが作られていそう）
	if pvc.UID != b.Spec.PVC.UID {
		logger.Info(fmt.Sprintf(
			"Skip processing because pvc.UID='%s' is different from fluentpvcbinding.Spec.PVC.UID='%s' for name='%s'.",
			pvc.UID, b.Spec.PVC.UID, pvc.Name,
		))
		return ctrl.Result{}, nil
	}

	// FluentPVCBinding の Condition が Unknown の場合、処理対象外とする
	if b.IsConditionUnknown() {
		logger.Info(fmt.Sprintf("fluentpvcbinding='%s' is unknown status, so skip processing.", b.Name))
		return ctrl.Result{}, nil
	}

	// FluentPVCBinding の Condition が OutOfUse ではない場合、処理対象外とする
	if !b.IsConditionOutOfUse() {
		logger.Info(fmt.Sprintf("fluentpvcbinding='%s' is not out of use yet.", b.Name))
		return requeueResult(10 * time.Second), nil
	}

	// OutOfUse の場合 PVC を使っていない
	// finalizer job の実行 または 削除

	logger.Info(fmt.Sprintf(
		"pvc='%s' is finalizing because the status of fluentpvcbinding='%s' is OutOfUse.",
		pvc.Name, b.Name,
	))

	// FluentPVCBinding の Condition が FinalizerJobApplied ではない場合、 Finalizer Job を Apply する
	// ここではもう OutOfUse しかない気がする
	if !b.IsConditionFinalizerJobApplied() {
		jobs := &batchv1.JobList{}

		// FlunetPVCBinding を Owner とする Job をあってもなくても取得
		if err := r.List(ctx, jobs, matchingOwnerControllerField(b.Name)); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
		}

		// Job があれば　FluentPVCBinding の状態更新が遅延しているだけなので処理対象外とする
		if len(jobs.Items) != 0 {
			logger.Info(fmt.Sprintf(
				"fluentpvcbinding='%s' status indicates any finalizer job is not applied, but some jobs are found: %+v",
				b.Name, jobs.Items,
			))
			return requeueResult(10 * time.Second), nil
		}



		// job がなければ job を作成する
		// Finalizer Job は Fluent PVC で定義された JobSpec に処理対象の PVC を Mount して Apply する
		fpvc := &fluentpvcv1alpha1.FluentPVC{}
		if err := r.Get(ctx, client.ObjectKey{Name: metav1.GetControllerOf(b).Name}, fpvc); err != nil {
			return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
		}

		// PVC Auto Finalization: Pod 削除後 PVC 内のデータを処理するための Job を自動で発行し、Job が成功したら PVC を削除します。
		// Apply the finalizer Job for the PVC.
		// Pod から利用されなくなったら Finalizer Job を発行する。
		// Finalizer Job は FluentPVCBinding 及び PVC と同名とし、2つ以上の Job が実行されないようにする。
		j := &batchv1.Job{}
		j.SetName(b.Name)
		j.SetNamespace(b.Namespace)
		if _, err := ctrl.CreateOrUpdate(ctx, r.Client, j, func() error {
			j.Spec = *fpvc.Spec.PVCFinalizerJobSpecTemplate.DeepCopy()

			// jobを作るタイミングで、共通のVolumeを注入する
			// シークレット情報
			for _, v := range fpvc.Spec.CommonVolumes {
				podutils.InjectOrReplaceVolume(&j.Spec.Template.Spec, v.DeepCopy())
			}
			// fluent-pvc-operator用のVolumeMount
			podutils.InjectOrReplaceVolume(&j.Spec.Template.Spec, &corev1.Volume{
				Name: fpvc.Spec.PVCVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvc.Name,
					},
				},
			})

			// jobを作るタイミングで、共通のVolumeMountを注入する
			// シークレット情報
			for _, vm := range fpvc.Spec.CommonVolumeMounts {
				podutils.InjectOrReplaceVolumeMount(&j.Spec.Template.Spec, vm.DeepCopy())
			}
			// fluent-pvc-operator用のVolumeMount
			podutils.InjectOrReplaceVolumeMount(&j.Spec.Template.Spec, &corev1.VolumeMount{
				Name:      fpvc.Spec.PVCVolumeName,
				MountPath: fpvc.Spec.PVCVolumeMountPath,
			})

			// jobを作るタイミングで、共通のEnvを注入する
			for _, e := range fpvc.Spec.CommonEnvs {
				podutils.InjectOrReplaceEnv(&j.Spec.Template.Spec, e.DeepCopy())
			}
			return ctrl.SetControllerReference(b, j, r.Scheme)
		}); err != nil {
			return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
		}
	}

	// FinalizerJobAppliedなら処理中
	// FluentPVCBinding の Condition が FinalizerJobApplied ではあるが FinalizerJobSucceeded もしくは FinalizerJobFailed ではない場合、FinalizerJobSucceeded もしくは FinalizerJobFailed となるまで処理対象外とする。
	if !b.IsConditionFinalizerJobSucceeded() && !b.IsConditionFinalizerJobFailed() {
		logger.Info(fmt.Sprintf(
			"pvc='%s' is finalizing by fluentpvcbinding='%s'.",
			pvc.Name, b.Name,
		))
		return requeueResult(10 * time.Second), nil
	}

	// jobが失敗だった場合、やり直しを見守る
	// FluentPVCBinding の Condition が FinalizerJobFailed である場合、処理対象外とする。
	if b.IsConditionFinalizerJobFailed() {
		logger.Info(fmt.Sprintf("Skip processing because the finalizer job='%s' is failed.", b.Name))
		return requeueResult(10 * time.Second), nil
	}

	// FluentPVCBinding の Condition が FinalizerJobSucceeded である場合、PVC に指定した Finalizer fluent-pvc-operator.tech.zozo.com/pvc-protection を削除し、 PVC も削除する

	// Finalizer Job が成功したら PVC から Finalizer を削除する。
	logger.Info(fmt.Sprintf("Remove the finalizer='%s' from pvc='%s'", constants.PVCFinalizerName, pvc.Name))
	controllerutil.RemoveFinalizer(pvc, constants.PVCFinalizerName)
	if err := r.Update(ctx, pvc); client.IgnoreNotFound(err) != nil {
		// pvc がコンフリクトしたら見守る
		if apierrors.IsConflict(err) {
			// NOTE: Conflict with deleting the pvc in other pvcReconciler#Reconcile.
			return requeueResult(10 * time.Second), nil
		}
		return ctrl.Result{}, xerrors.Errorf(
			"Failed to remove finalizer from PVC='%s'.: %w",
			pvc.Name, err,
		)
	}

	// PVC を削除する
	// Delete the PVC when the finalizer Job is succeeded.
	logger.Info(fmt.Sprintf("Delete pvc='%s' because it is finalized.", pvc.Name))
	if err := r.Delete(ctx, pvc, deleteOptionsBackground(&pvc.UID, &pvc.ResourceVersion)); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, xerrors.Errorf("Unexpected error occurred.: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *pvcReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pred := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}

	// NOTE: Avoid 'indexer conflict: map[field:.metadata.ownerReference.controller:{}]'
	// ctx := context.Background()
	// if err := mgr.GetFieldIndexer().IndexField(ctx,
	// 	&batchv1.Job{},
	// 	constants.OwnerControllerField,
	// 	indexJobByOwnerFluentPVCBinding,
	// ); err != nil {
	// 	return xerrors.Errorf("Unexpected error occurred.: %w", err)
	// }

	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(pred).
		For(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
