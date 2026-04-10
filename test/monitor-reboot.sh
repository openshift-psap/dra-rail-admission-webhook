#!/bin/bash
# Monitor webhook health during GPU node rolling reboot
# Usage: ./test/monitor-reboot.sh [kubeconfig]

KUBECONFIG="${1:-${KUBECONFIG:-$HOME/.kube/config}}"
export KUBECONFIG
LOG_DIR="/tmp/webhook-reboot-monitor-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$LOG_DIR"

echo "=== Webhook Reboot Monitor ==="
echo "Logs: $LOG_DIR"
echo "Press Ctrl+C to stop"
echo ""

# --- 1. Webhook + reconciler pod status (every 5s) ---
(
while true; do
    echo "--- $(date -Iseconds) ---"
    kubectl get pods -n dra-webhook-system -o wide 2>&1
    echo ""
    sleep 5
done
) > "$LOG_DIR/pod-status.log" 2>&1 &
PID_PODS=$!

# --- 2. Webhook logs (streaming) ---
kubectl logs -n dra-webhook-system -l app=dra-gpu-nic-webhook --all-containers -f --prefix 2>/dev/null \
  > "$LOG_DIR/webhook-logs.log" 2>&1 &
PID_WHLOGS=$!

# --- 3. Reconciler logs (streaming) ---
kubectl logs -n dra-webhook-system -l app=dra-gpu-nic-reconciler --all-containers -f --prefix 2>/dev/null \
  > "$LOG_DIR/reconciler-logs.log" 2>&1 &
PID_RECLOGS=$!

# --- 4. GPU node status (every 10s) ---
(
while true; do
    echo "--- $(date -Iseconds) ---"
    kubectl get nodes -l node-role.kubernetes.io/worker -o wide 2>&1 | grep -E 'NAME|worker-3'
    echo ""
    sleep 10
done
) > "$LOG_DIR/node-status.log" 2>&1 &
PID_NODES=$!

# --- 5. ResourceSlice count per node (every 10s) ---
(
while true; do
    echo "--- $(date -Iseconds) ---"
    kubectl get resourceslices -o json 2>/dev/null | python3 -c "
import json, sys
try:
    data = json.load(sys.stdin)
    nodes = {}
    for s in data.get('items',[]):
        drv = s['spec'].get('driver','')
        node = s['spec'].get('nodeName','?')
        devs = len(s['spec'].get('devices',[]))
        key = f'{node}/{drv}'
        nodes[key] = nodes.get(key, 0) + devs
    for k in sorted(nodes):
        print(f'  {k}: {nodes[k]} devices')
except: pass
" 2>/dev/null
    echo ""
    sleep 10
done
) > "$LOG_DIR/resourceslices.log" 2>&1 &
PID_SLICES=$!

# --- 6. Admission probe: try creating a GPU-NIC pod every 15s ---
(
# Ensure probe namespace exists
kubectl apply -f - <<EOF 2>/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: reboot-probe
  labels:
    dra.llm-d.io/webhook-enabled: "true"
EOF

SEQ=0
while true; do
    TS=$(date -Iseconds)
    NAME="probe-${SEQ}"
    # Try creating a 1-pair pod
    RESULT=$(kubectl apply -n reboot-probe -f - 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $NAME
spec:
  restartPolicy: Never
  containers:
  - name: test
    image: registry.k8s.io/pause:3.10
    resources:
      requests:
        dra.llm-d.io/gpu-nic-pair: "1"
      limits:
        dra.llm-d.io/gpu-nic-pair: "1"
EOF
    )

    if echo "$RESULT" | grep -q "created"; then
        # Check if it was mutated
        MUTATED=$(kubectl get pod "$NAME" -n reboot-probe -o jsonpath='{.metadata.annotations.dra\.llm-d\.io/mutated}' 2>/dev/null)
        CLAIMS=$(kubectl get pod "$NAME" -n reboot-probe -o jsonpath='{.spec.resourceClaims}' 2>/dev/null)
        if [ "$MUTATED" = "true" ] && [ -n "$CLAIMS" ] && [ "$CLAIMS" != "null" ]; then
            echo "$TS ADMIT  probe-${SEQ} (mutated=true, claims present)"
        else
            echo "$TS ADMIT  probe-${SEQ} (mutated=${MUTATED:-missing}, WARNING: may not be mutated)"
        fi
        # Clean up
        kubectl delete pod "$NAME" -n reboot-probe --grace-period=0 --wait=false 2>/dev/null
    else
        echo "$TS REJECT probe-${SEQ}: $RESULT"
    fi

    SEQ=$((SEQ + 1))
    sleep 15
done
) > "$LOG_DIR/admission-probe.log" 2>&1 &
PID_PROBE=$!

# --- 7. Event watcher for dra-webhook-system ---
kubectl get events -n dra-webhook-system --watch-only 2>/dev/null \
  > "$LOG_DIR/events.log" 2>&1 &
PID_EVENTS=$!

# --- Summary display: tail key logs ---
echo "Monitoring started. Tailing admission probe..."
echo "(Full logs in $LOG_DIR/)"
echo ""
tail -f "$LOG_DIR/admission-probe.log" &
PID_TAIL=$!

cleanup() {
    echo ""
    echo "Stopping monitors..."
    kill $PID_PODS $PID_WHLOGS $PID_RECLOGS $PID_NODES $PID_SLICES $PID_PROBE $PID_EVENTS $PID_TAIL 2>/dev/null
    kubectl delete namespace reboot-probe --wait=false 2>/dev/null
    echo ""
    echo "=== Monitor Summary ==="
    echo "Logs saved to: $LOG_DIR/"
    echo "  pod-status.log       — webhook/reconciler pod health"
    echo "  webhook-logs.log     — webhook container logs"
    echo "  reconciler-logs.log  — reconciler container logs"
    echo "  node-status.log      — GPU node Ready/NotReady"
    echo "  resourceslices.log   — device count per node"
    echo "  admission-probe.log  — pod admission success/failure"
    echo "  events.log           — namespace events"
    echo ""
    echo "Quick analysis:"
    echo "  Admission failures: $(grep -c 'REJECT' "$LOG_DIR/admission-probe.log" 2>/dev/null || echo 0)"
    echo "  Successful admits:  $(grep -c 'ADMIT' "$LOG_DIR/admission-probe.log" 2>/dev/null || echo 0)"
    echo "  Unmutated admits:   $(grep -c 'WARNING' "$LOG_DIR/admission-probe.log" 2>/dev/null || echo 0)"
}
trap cleanup EXIT INT TERM

wait
