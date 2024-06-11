package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	// オリジナル
	fluentpvcv1alpha1 "github.com/st-tech/fluent-pvc-operator/api/v1alpha1"
	"github.com/st-tech/fluent-pvc-operator/constants"
	hashutils "github.com/st-tech/fluent-pvc-operator/utils/hash"
	podutils "github.com/st-tech/fluent-pvc-operator/utils/pod"
)

//+kubebuilder:webhook:path=/pod/mutate,mutating=true,failurePolicy=fail,sideEffects=None,groups=core,resources=pods,verbs=create,versions=v1,name=pod-mutation-webhook.fluent-pvc-operator.tech.zozo.com,admissionReviewVersions={v1,v1beta1}
//+kubebuilder:webhook:path=/pod/validate,mutating=false,failurePolicy=fail,sideEffects=None,groups=core,resources=pods,verbs=create,versions=v1,name=pod-validation-webhook.fluent-pvc-operator.tech.zozo.com,admissionReviewVersions={v1,v1beta1}

//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;delete

func PodAdmissionResponse(pod *corev1.Pod, req admission.Request) admission.Response {
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// パスにValidateとMutateの2つのWebhookを登録する
func SetupPodWebhookWithManager(mgr ctrl.Manager) error {
	mgr.GetWebhookServer().Register("/pod/validate", &webhook.Admission{Handler: NewPodValidator(mgr.GetClient())})
	mgr.GetWebhookServer().Register("/pod/mutate", &webhook.Admission{Handler: NewPodMutator(mgr.GetClient())})
	return nil
}

// *Mutator* //

type podMutator struct {
	client.Client
	decoder *admission.Decoder
}

// managerのclient登録。Handlerを返す
func NewPodMutator(c client.Client) admission.Handler {
	return &podMutator{Client: c}
}

// Pod 作成時に実行される Admission Webhook が実装されている。
func (m *podMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := ctrl.LoggerFrom(ctx).WithName("podMutator").WithName("Handle")

	// リクエストをオブジェクトに変換
	pod := &corev1.Pod{}
	if err := m.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// DryRunの場合、サイドカーコンテナとして扱う（テストが通らないため、このような処理をここに入れている）
	if req.DryRun != nil && *req.DryRun {
		return admission.Allowed("It' sidecar container.")
	}

	// debug: podマニフェストの中身を確認
	marshaledPod, err := json.MarshalIndent(pod, "", "  ")
	if err != nil {
		fmt.Println("error")
	}
	fmt.Printf("%s\n", marshaledPod)
	if len(pod.Spec.Containers) == 0 {
		return admission.Denied("pod has no containers")
	}

	// FluentPVCBinding オブジェクト作成
	fpvc := &fluentpvcv1alpha1.FluentPVC{}
	// pod annotationの特定キーの値を確認
	// annotation で fluent-pvc-operator.tech.zozo.com/fluent-pvc-name: hogehoge が定義されている Pod を処理対象とする
	if fpvcName, err := pod.Labels[constants.PodLabelFluentPVCName]; err {
		// pod annotationに特定キーが存在しない場合、エラーを返す
		// admission.Deniedで返す必要ある?
		// fluent-pvc-operator.tech.zozo.com/fluent-pvc-name の value で指定された FluentPVC が定義されていない場合エラーとする
		return admission.Denied(fmt.Sprintf("pod does not have %s label.", constants.PodLabelFluentPVCName))
	} else {
		// pod annotationに特定キーが存在する場合、FluentPVC オブジェクトを取得する

		// debug:リクエストの中身を確認
		marshaledReq, err := json.MarshalIndent(req, "", "  ")
		if err != nil {
			fmt.Println("error")
		}
		fmt.Printf("%s", marshaledReq)

		if req.DryRun != nil && *req.DryRun {
			// DryRunの場合、何もしない
			// Nothing to do
		} else {
			// DryRunでない場合、FluentPVC オブジェクトを取得する
			if err := m.Get(ctx, client.ObjectKey{Name: fpvcName}, fpvc); err != nil {
				return admission.Errored(http.StatusInternalServerError, err)
			}
		}
	}

	// TODO: Consider too long fluent-pvc name
	collisionCount := int32(rand.IntnRange(math.MinInt32, math.MaxInt32)) // Using the count for collision avoidance
	fpvcbName := fmt.Sprintf(
		"%s-%s-%s",
		fpvc.Name, hashutils.ComputeHash(fpvc, nil), hashutils.ComputeHash(pod, &collisionCount),
	)

	// Create a PVC for the Pod.
	// fluent-pvc-operator.tech.zozo.com/fluent-pvc-name の value で指定された FluentPVC が定義が存在していた場合、その定義に則って PVC の作成、及び PVC と Sidecar Container の Pod Manifest への Injection を行う
	logger.Info(fmt.Sprintf("Createing PVC='%s'(namespace='%s').", fpvcbName, req.Namespace))

	pvc := &corev1.PersistentVolumeClaim{}
	pvc.SetName(fpvcbName)
	pvc.SetNamespace(req.Namespace)
	pvc.Spec = *fpvc.Spec.PVCSpecTemplate.DeepCopy()
	// PVC は fluent-pvc-operator が処理完了するまで削除されないよう、 Finalizer fluent-pvc-operator.tech.zozo.com/pvc-protection を指定しておく。
	controllerutil.AddFinalizer(pvc, constants.PVCFinalizerName)
	// NOTE: fluentpvcbinding does not own pvc for preventing pvc from becoming terminating when fluentpvcbinding
	//       is deleted. This is because the finalizer job cannot mount the pvc if it is terminating.

	// PVCの作成
	if err := m.Create(ctx, pvc, &client.CreateOptions{}); err != nil {
		logger.Error(err, fmt.Sprintf("Cannot Create PVC='%s'(namespace='%s').", fpvcbName, req.Namespace))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// 作成された PVC, PVC を Inject した Pod を管理する FluentPVCBinding も同時に生成する。
	// PVC と FluentPVCBinding の名称は同一とする
	logger.Info(fmt.Sprintf("Create FluentPVCBinding='%s'(namespace='%s').", fpvcbName, req.Namespace))
	b := &fluentpvcv1alpha1.FluentPVCBinding{}
	b.SetName(fpvcbName)
	b.SetNamespace(req.Namespace)

	// FluentPVCBinding は FluentPVC, Pod, PVC の Name 及び UID を保持する
	// ただし、 pod_webhook 到達時点の Pod は Schedule されておらず UID を保持していないため、後述の fluentpvcbinding_controller で最初に発見された同一名称 Pod の UID を保持するようにする。

	b.SetFluentPVC(fpvc)
	b.SetPod(pod)
	b.SetPVC(pvc)
	// FluentPVCBinding は fluent-pvc-operator.tech.zozo.com/fluentpvcbinding-protection を指定しておく。
	controllerutil.AddFinalizer(b, constants.FluentPVCBindingFinalizerName)
	// FluentPVCBinding は FluentPVC を Owner Controller とする
	if err := ctrl.SetControllerReference(fpvc, b, m.Scheme()); err != nil {
		logger.Error(err, fmt.Sprintf("Cannot set FluentPVC as a Controller OwnerReference on owned for FluentPVCBinding='%s'.", fpvcbName))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// FluentPVCBindingの作成
	if err := m.Create(ctx, b, &client.CreateOptions{}); err != nil {
		logger.Error(err, fmt.Sprintf("Cannot Create FluentPVCBinding='%s'.", fpvcbName))
		return admission.Errored(http.StatusInternalServerError, err)
	}
	// フェースの設定・更新
	b.SetPhasePending()
	if err := m.Status().Update(ctx, b); err != nil {
		logger.Error(err, fmt.Sprintf("Cannot update the status of FluentPVCBinding='%s'.", fpvcbName))
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Dynamic PVC Provisioning: Admission Webhook によって Pod 作成時に PVC を生成し Manifest へ Inject します。
	// Inject the PVC to the Pod Manifest.
	// PVC を作成し Pod Manifest へ Inject する
	logger.Info(fmt.Sprintf(
		"Inject PVC='%s' into Pod='%s'(namespace='%s', generatorName='%s').",
		fpvcbName, pod.Name, req.Namespace, pod.GenerateName,
	))
	podPatched := pod.DeepCopy()
	if podPatched.Labels == nil {
		podPatched.Labels = map[string]string{}
	}
	// podにFluentPVCBindingの名前も追加（FluentPVCの名前は最初から追加されているはず）
	podPatched.Labels[constants.PodLabelFluentPVCBindingName] = fpvcbName

	// podを作るタイミングで、共通のVolumeを注入する
	// シークレット情報
	for _, v := range fpvc.Spec.CommonVolumes {
		podutils.InjectOrReplaceVolume(&podPatched.Spec, v.DeepCopy())
	}
	// fluent-pvc-operator用のVolume
	podutils.InjectOrReplaceVolume(&podPatched.Spec, &corev1.Volume{
		Name: fpvc.Spec.PVCVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fpvcbName,
			},
		},
	})

	// Sidecar Container Injection: Admission Webhook によって Pod 作成時に任意の Container 定義を Manifest へ Inject します。
	// Inject the Sidecar Container Definition to the Pod Manifest.
	// Sidecar Container の定義を Pod Manifest へ Inject する。
	podutils.InjectOrReplaceContainer(&podPatched.Spec, fpvc.Spec.SidecarContainerTemplate.DeepCopy())

	// podを作るタイミングで、共通のVolumeMountを注入する
	// シークレット情報
	for _, vm := range fpvc.Spec.CommonVolumeMounts {
		podutils.InjectOrReplaceVolumeMount(&podPatched.Spec, vm.DeepCopy())
	}
	// fluent-pvc-operator用のVolumeMount
	podutils.InjectOrReplaceVolumeMount(&podPatched.Spec, &corev1.VolumeMount{
		Name:      fpvc.Spec.PVCVolumeName,
		MountPath: fpvc.Spec.PVCVolumeMountPath,
	})

	// target podに共通のEnvを注入
	for _, e := range fpvc.Spec.CommonEnvs {
		podutils.InjectOrReplaceEnv(&podPatched.Spec, e.DeepCopy())
	}

	logger.Info(fmt.Sprintf(
		"Patch Pod='%s'(namespace='%s', generatorName='%s') with PVC='%s' and FluentPVCBinding='%s' by FluentPVC='%s'.",
		podPatched.Name, req.Namespace, podPatched.GenerateName, pvc.Name, b.Name, fpvc.Name,
	))
	return PodAdmissionResponse(podPatched, req)
}

// decoder登録
func (m *podMutator) InjectDecoder(d *admission.Decoder) error {
	m.decoder = d
	return nil
}

// *Validator* //

type podValidator struct {
	Client  client.Client
	decoder *admission.Decoder
}

// managerのclient登録。Handlerを返す
func NewPodValidator(c client.Client) admission.Handler {
	return &podValidator{Client: c}
}

func (v *podValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}

	err := v.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// TODO: implement
	// key := "example-mutating-admission-webhook"
	// anno, found := pod.Annotations[key]
	// if !found {
	// 	return admission.Denied(fmt.Sprintf("missing annotation %s", key))
	// }
	// if anno != "foo" {
	// 	return admission.Denied(fmt.Sprintf("annotation %s did not have value %q", key, "foo"))
	// }

	return admission.Allowed("")
}

func (v *podValidator) InjectDecoder(d *admission.Decoder) error {
	v.decoder = d
	return nil
}
