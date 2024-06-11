package constants

const (
	OwnerControllerField          = ".metadata.ownerReference.controller"
	FluentPVCFinalizerName        = "fluent-pvc-operator.tech.zozo.com/fluentpvc-protection"
	FluentPVCBindingFinalizerName = "fluent-pvc-operator.tech.zozo.com/fluentpvcbinding-protection"
	PVCFinalizerName              = "fluent-pvc-operator.tech.zozo.com/pvc-protection"
	PodLabelFluentPVCName         = "fluent-pvc-operator.tech.zozo.com/fluent-pvc-name"
	PodLabelFluentPVCBindingName  = "fluent-pvc-operator.tech.zozo.com/fluent-pvc-binding-name"
)
