package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/klog/v2"
)

var (
	runtimeScheme = func() *runtime.Scheme {
		s := runtime.NewScheme()
		_ = admissionv1.AddToScheme(s)
		_ = corev1.AddToScheme(s)
		return s
	}()
	codecs       = serializer.NewCodecFactory(runtimeScheme)
	deserializer = codecs.UniversalDeserializer()
)

// Handler is the HTTP handler for the mutating admission webhook.
type Handler struct {
	Mutator *Mutator
}

// ServeHTTP handles the admission review request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST is allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.ErrorS(err, "Failed to read request body")
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		http.Error(w, fmt.Sprintf("unsupported content type %q, expected application/json", contentType), http.StatusUnsupportedMediaType)
		return
	}

	// Decode the AdmissionReview
	var admissionReview admissionv1.AdmissionReview
	if _, _, err := deserializer.Decode(body, nil, &admissionReview); err != nil {
		klog.ErrorS(err, "Failed to decode admission review")
		http.Error(w, fmt.Sprintf("failed to decode: %v", err), http.StatusBadRequest)
		return
	}

	if admissionReview.Request == nil {
		http.Error(w, "admission review has no request", http.StatusBadRequest)
		return
	}

	response := h.handleAdmission(r.Context(), admissionReview.Request)

	admissionReview.Response = response
	admissionReview.Response.UID = admissionReview.Request.UID

	respBytes, err := json.Marshal(admissionReview)
	if err != nil {
		klog.ErrorS(err, "Failed to marshal admission response")
		http.Error(w, fmt.Sprintf("failed to marshal response: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(respBytes)
}

// handleAdmission processes a single admission request and returns a response.
func (h *Handler) handleAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Only handle Pod CREATE
	if req.Resource.Resource != "pods" || req.Operation != admissionv1.Create {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	// Decode the pod
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.ErrorS(err, "Failed to unmarshal pod")
		return denyResponse(fmt.Sprintf("failed to unmarshal pod: %v", err))
	}

	// Mutate
	patch, err := h.Mutator.Mutate(ctx, &pod, req.Namespace)
	if err != nil {
		klog.ErrorS(err, "Mutation denied", "namespace", req.Namespace, "pod", podName(&pod))
		return denyResponse(err.Error())
	}

	if patch == nil {
		// No mutation needed
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patch,
		PatchType: &patchType,
	}
}

func denyResponse(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result: &metav1.Status{
			Status:  "Failure",
			Message: message,
			Code:    http.StatusForbidden,
		},
	}
}
