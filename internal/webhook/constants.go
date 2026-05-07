package webhook

const (
	// defaultRulePriority is the Linux policy routing rule priority used
	// for source-based rules. 32765 is just below the default "lookup main"
	// rule (32766), ensuring rail-specific policy tables are consulted for
	// traffic originating from a rail's subnet.
	defaultRulePriority = 32765

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

	// EncapsulationAttribute is the DRA device attribute indicating network transport.
	// Values: "infiniband" for IB, "ether" for Ethernet.
	EncapsulationAttribute = "dra.net/encapsulation"

	// NICPCIAddressAttribute is the DRA device attribute for NIC PCI bus address.
	NICPCIAddressAttribute = "dra.net/pciAddress"

	// GPUPCIBusIDAttribute is the DRA device attribute for GPU PCI bus ID.
	GPUPCIBusIDAttribute = "resource.kubernetes.io/pciBusID"

	// ResourceClaimName is the name used in pod.spec.resourceClaims[].name
	ResourceClaimName = "gpu-nic-devices"

	// AnnotationOrphanedAt marks a resource as orphaned with a timestamp.
	AnnotationOrphanedAt = "dra.llm-d.io/orphaned-at"

	// MutatePath is the HTTP path for the full webhook handler (labeled namespaces).
	MutatePath = "/mutate"

	// MutateExtPath is the HTTP path for the extended-resource-only handler.
	// Serves all non-system namespaces; only processes intercepted resources.
	MutateExtPath = "/mutate-ext"

	// ResourceNvidiaGPU is the standard Kubernetes extended resource name for NVIDIA GPUs.
	ResourceNvidiaGPU = "nvidia.com/gpu"
)
