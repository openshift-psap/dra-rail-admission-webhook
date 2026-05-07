package webhook

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// MutateFunc is a function that mutates a pod and returns a JSON patch.
type MutateFunc func(ctx context.Context, pod *corev1.Pod, namespace string) ([]byte, error)

// mutationRequest represents a queued pod mutation.
type mutationRequest struct {
	ctx       context.Context
	pod       *corev1.Pod
	namespace string
	count     int // GPU-NIC pair count, used as priority (higher = first)
	mutateFn  MutateFunc
	resultCh  chan<- mutationResult
}

// mutationResult is the response sent back to the waiting handler.
type mutationResult struct {
	patch []byte
	err   error
}

// MutationQueue collects concurrent admission requests and processes them
// in priority order (highest GPU-NIC pair count first). This ensures that
// larger, more constrained requests get first pick of rails.
//
// When a request arrives:
//  1. It is added to the pending batch.
//  2. A short debounce timer starts (or resets) to collect concurrent requests.
//  3. When the timer fires, the batch is sorted by count (descending) and
//     processed serially. Each Mutate() call sees the templates created by
//     prior calls, preventing rail collisions.
type MutationQueue struct {
	mutator  *Mutator
	debounce time.Duration

	mu         sync.Mutex
	batch      []*mutationRequest
	timer      *time.Timer
	processing bool
}

// NewMutationQueue creates a queue with the given debounce window.
// A debounce of 50-100ms is recommended: long enough to catch a burst
// of concurrent pod creations, short enough to be imperceptible.
func NewMutationQueue(mutator *Mutator, debounce time.Duration) *MutationQueue {
	return &MutationQueue{
		mutator:  mutator,
		debounce: debounce,
	}
}

// Enqueue adds a mutation request to the queue and blocks until it is processed.
// Uses the default Mutator.Mutate function.
func (q *MutationQueue) Enqueue(ctx context.Context, pod *corev1.Pod, namespace string, count int) ([]byte, error) {
	return q.EnqueueFunc(ctx, pod, namespace, count, q.mutator.Mutate)
}

// EnqueueFunc adds a mutation request with a custom mutation function.
func (q *MutationQueue) EnqueueFunc(ctx context.Context, pod *corev1.Pod, namespace string, count int, fn MutateFunc) ([]byte, error) {
	ch := make(chan mutationResult, 1)
	req := &mutationRequest{
		ctx:       ctx,
		pod:       pod,
		namespace: namespace,
		count:     count,
		mutateFn:  fn,
		resultCh:  ch,
	}

	q.mu.Lock()
	q.batch = append(q.batch, req)

	// Always reset the debounce timer to collect more requests, even
	// during processing. This ensures a burst of sequential requests
	// (e.g., from kubectl apply) gets batched together so that the
	// priority queue can give larger requests first pick of rails.
	if q.timer != nil {
		q.timer.Stop()
	}
	q.timer = time.AfterFunc(q.debounce, q.processBatch)
	q.mu.Unlock()

	result := <-ch
	return result.patch, result.err
}

// processBatch drains the queue in priority order. It loops until no new
// requests have arrived, so late arrivals during processing are included.
func (q *MutationQueue) processBatch() {
	q.mu.Lock()
	if q.processing {
		// Another processBatch goroutine is already running; it will
		// pick up our batch items in its next loop iteration.
		q.mu.Unlock()
		return
	}
	q.processing = true
	q.mu.Unlock()

	for {
		q.mu.Lock()
		if len(q.batch) == 0 {
			q.processing = false
			q.timer = nil
			q.mu.Unlock()
			return
		}

		// Drain current batch
		batch := q.batch
		q.batch = nil
		q.mu.Unlock()

		// Sort by count descending (largest requests first)
		sort.Slice(batch, func(i, j int) bool {
			return batch[i].count > batch[j].count
		})

		klog.V(2).InfoS("Processing mutation batch",
			"batchSize", len(batch),
			"priorities", batchCounts(batch))

		// Process serially — each mutation creates templates visible to the next
		for _, req := range batch {
			patch, err := req.mutateFn(req.ctx, req.pod, req.namespace)
			req.resultCh <- mutationResult{patch: patch, err: err}
		}
	}
}

// batchCounts returns the count values for logging.
func batchCounts(batch []*mutationRequest) []int {
	counts := make([]int, len(batch))
	for i, r := range batch {
		counts[i] = r.count
	}
	return counts
}
