package webhook

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

var cidrPoolGVR = schema.GroupVersionResource{
	Group:    "nv-ipam.nvidia.com",
	Version:  "v1alpha1",
	Resource: "cidrpools",
}

type CIDRPoolAllocation struct {
	Prefix      string
	Gateway     string
	ReservedIPs []string
}

type CIDRPoolCache struct {
	mu     sync.RWMutex
	data   map[int]map[string]CIDRPoolAllocation
	client dynamic.Interface
	pools  []CIDRPoolRef
	stopCh chan struct{}
}

func NewCIDRPoolCache(client dynamic.Interface, pools []CIDRPoolRef) *CIDRPoolCache {
	return &CIDRPoolCache{
		data:   make(map[int]map[string]CIDRPoolAllocation),
		client: client,
		pools:  pools,
		stopCh: make(chan struct{}),
	}
}

func (c *CIDRPoolCache) Start(ctx context.Context, refreshInterval time.Duration) error {
	if err := c.refresh(ctx); err != nil {
		return fmt.Errorf("initial CIDRPool refresh failed: %w", err)
	}

	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := c.refresh(ctx); err != nil {
					klog.Warningf("CIDRPool refresh failed: %v", err)
				}
			case <-c.stopCh:
				return
			}
		}
	}()

	return nil
}

func (c *CIDRPoolCache) Stop() {
	close(c.stopCh)
}

func (c *CIDRPoolCache) refresh(ctx context.Context) error {
	newData := make(map[int]map[string]CIDRPoolAllocation)
	poolsRefreshed := 0

	for _, pool := range c.pools {
		obj, err := c.client.Resource(cidrPoolGVR).Namespace(pool.Namespace).Get(ctx, pool.Name, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Failed to get CIDRPool %s/%s: %v", pool.Namespace, pool.Name, err)
			continue
		}

		allocations, err := c.parseAllocations(obj, pool.RailIndex)
		if err != nil {
			klog.Warningf("Failed to parse CIDRPool %s/%s: %v", pool.Namespace, pool.Name, err)
			continue
		}

		if _, ok := newData[pool.RailIndex]; !ok {
			newData[pool.RailIndex] = make(map[string]CIDRPoolAllocation)
		}

		for nodeName, alloc := range allocations {
			newData[pool.RailIndex][nodeName] = alloc
		}

		poolsRefreshed++
	}

	if poolsRefreshed > 0 {
		c.mu.Lock()
		c.data = newData
		c.mu.Unlock()
		klog.Infof("CIDRPool cache refreshed: %d pools, %d rails at %s", poolsRefreshed, len(newData), time.Now().Format(time.RFC3339))
	}

	return nil
}

func (c *CIDRPoolCache) parseAllocations(obj *unstructured.Unstructured, railIndex int) (map[string]CIDRPoolAllocation, error) {
	allocations := make(map[string]CIDRPoolAllocation)

	statusAllocations, found, err := unstructured.NestedSlice(obj.Object, "status", "allocations")
	if err != nil {
		return nil, fmt.Errorf("error reading status.allocations: %w", err)
	}
	if !found {
		return allocations, nil
	}

	var perNodeExclusions []map[string]interface{}
	exclusionsRaw, found, err := unstructured.NestedSlice(obj.Object, "spec", "perNodeExclusions")
	if err == nil && found {
		for _, e := range exclusionsRaw {
			if m, ok := e.(map[string]interface{}); ok {
				perNodeExclusions = append(perNodeExclusions, m)
			}
		}
	}

	gatewayIndex, _, _ := unstructured.NestedInt64(obj.Object, "spec", "gatewayIndex")

	for _, allocRaw := range statusAllocations {
		allocMap, ok := allocRaw.(map[string]interface{})
		if !ok {
			continue
		}

		nodeName, _, _ := unstructured.NestedString(allocMap, "nodeName")
		prefix, _, _ := unstructured.NestedString(allocMap, "prefix")
		gateway, _, _ := unstructured.NestedString(allocMap, "gateway")

		if nodeName == "" || prefix == "" {
			continue
		}

		reservedIPs, err := computeReservedIPs(prefix, perNodeExclusions, int(gatewayIndex))
		if err != nil {
			klog.Warningf("Failed to compute reserved IPs for node %s, prefix %s: %v", nodeName, prefix, err)
			reservedIPs = []string{}
		}

		allocations[nodeName] = CIDRPoolAllocation{
			Prefix:      prefix,
			Gateway:     gateway,
			ReservedIPs: reservedIPs,
		}
	}

	return allocations, nil
}

func (c *CIDRPoolCache) GetAllocation(railIndex int, nodeName string) (CIDRPoolAllocation, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	railData, ok := c.data[railIndex]
	if !ok {
		return CIDRPoolAllocation{}, false
	}

	alloc, ok := railData[nodeName]
	return alloc, ok
}

func (c *CIDRPoolCache) GetGateway(railIndex int, nodeName string) (string, error) {
	alloc, ok := c.GetAllocation(railIndex, nodeName)
	if !ok {
		return "", fmt.Errorf("no allocation for rail %d, node %s", railIndex, nodeName)
	}
	return alloc.Gateway, nil
}

func computeReservedIPs(prefix string, perNodeExclusions []map[string]interface{}, gatewayIndex int) ([]string, error) {
	_, ipNet, err := net.ParseCIDR(prefix)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR: %w", err)
	}

	reserved := make([]string, 0)

	for _, exclusion := range perNodeExclusions {
		startIndex, _, err := unstructured.NestedInt64(exclusion, "startIndex")
		if err != nil {
			continue
		}
		endIndex, _, err := unstructured.NestedInt64(exclusion, "endIndex")
		if err != nil {
			continue
		}

		for i := startIndex; i <= endIndex; i++ {
			ip := offsetIP(ipNet.IP, int(i))
			reserved = append(reserved, ip.String())
		}
	}

	if gatewayIndex > 0 {
		gatewayIP := offsetIP(ipNet.IP, gatewayIndex)
		reserved = append(reserved, gatewayIP.String())
	}

	return reserved, nil
}

func offsetIP(base net.IP, offset int) net.IP {
	ip := make(net.IP, len(base))
	copy(ip, base)

	for i := len(ip) - 1; i >= 0 && offset > 0; i-- {
		sum := int(ip[i]) + offset
		ip[i] = byte(sum & 0xFF)
		offset = sum >> 8
	}

	return ip
}
