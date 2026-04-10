package webhook

const (
	// ResourceGPUNICPair is the synthetic resource name users put in
	// container resources.requests to request GPU+NIC pairs.
	// Example: resources.requests["dra.llm-d.io/gpu-nic-pair"]: "4"
	ResourceGPUNICPair = "dra.llm-d.io/gpu-nic-pair"

	// AnnotationAllowCrossNUMA overrides NUMA zone enforcement when set to "true".
	AnnotationAllowCrossNUMA = "dra.llm-d.io/allow-cross-numa"

	// AnnotationMutated marks a pod as already processed by the webhook.
	AnnotationMutated = "dra.llm-d.io/mutated"

	// PCIeRootAttribute is the DRA device attribute used to pair GPU and NIC
	// on the same PCIe root complex.
	PCIeRootAttribute = "resource.kubernetes.io/pcieRoot"

	// NUMANodeAttribute is the DRA device attribute on NICs indicating NUMA zone.
	NUMANodeAttribute = "dra.net/numaNode"

	// ResourceClaimName is the name used in pod.spec.resourceClaims[].name
	ResourceClaimName = "gpu-nic-devices"

	// MutatePath is the HTTP path for the webhook handler.
	MutatePath = "/mutate"
)
