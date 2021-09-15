package native

import (
	"context"
	"github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/trace"
	"github.com/shirou/gopsutil/mem"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math"
	"runtime"
)

func totalResourceList(rs []corev1.ResourceList) corev1.ResourceList {
	total := make(map[corev1.ResourceName]resource.Quantity)
	for _, item := range rs {
		add(total, item)
	}
	return total
}

func add(total corev1.ResourceList, item corev1.ResourceList) {
	for name, quantity := range item {
		r, ok := total[name]
		if !ok {
			r = quantity
		} else {
			r.Add(quantity)
		}
		total[name] = r
	}
}

func sub(total corev1.ResourceList, item corev1.ResourceList) {
	for name, val := range item {
		r, ok := total[name]
		if !ok {
			continue
		}
		r.Sub(val)
		total[name] = r
	}
}

type NodeEventHandler struct {
	p              *Provider
	notifyNodeFunc func(*v1.Node)
	node           *v1.Node
	events         chan ProcessEvent
}

func (n *NodeEventHandler) start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.events:
				log.G(ctx).Debug("开始更新node status的 Allocatable/Capacity")

				n.notifyNodeFunc(n.node)
				log.G(ctx).Debug("完成node status的 Allocatable/Capacity 更新")
			}
		}
	}()
}

func (n *NodeEventHandler) configureNode(ctx context.Context, node *v1.Node) {
	_, span := trace.StartSpan(ctx, "NodeEventHandler.configureNode")
	defer span.End()

	node.Status.NodeInfo.KubeletVersion = n.p.initConfig.Version
	node.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: n.p.initConfig.InternalIP}}
	node.Status.Conditions = nodeConditions()
	node.Status.DaemonEndpoints = v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{
			Port: n.p.initConfig.DaemonPort,
		},
	}
	memory, err := mem.VirtualMemory()
	if err != nil {
		panic(err)
	}
	list := v1.ResourceList{
		v1.ResourceCPU:    *resource.NewMilliQuantity(int64(runtime.NumCPU()*1000), resource.DecimalSI),
		v1.ResourceMemory: *resource.NewQuantity(int64(memory.Available), resource.BinarySI),
		v1.ResourcePods:   *resource.NewQuantity(math.MaxInt64, resource.DecimalSI),
	}
	node.Status.Allocatable = list
	node.Status.Capacity = list
	node.Status.NodeInfo.OSImage = n.p.initConfig.Version
	node.Status.NodeInfo.KernelVersion = n.p.initConfig.Version
	node.Status.NodeInfo.OperatingSystem = "linux"
	node.Status.NodeInfo.Architecture = "amd64"
	node.ObjectMeta.Labels[v1.LabelArchStable] = "amd64"
	node.ObjectMeta.Labels[v1.LabelOSStable] = "linux"
	node.ObjectMeta.Labels["alpha.service-controller.kubernetes.io/exclude-balancer"] = "true"
	node.ObjectMeta.Labels["node.kubernetes.io/exclude-from-external-load-balancers"] = "true"

	n.node = node
	span.Logger().Debug("configureNode finish")
}

func nodeConditions() []corev1.NodeCondition {
	return []corev1.NodeCondition{
		{
			Type:               "Ready",
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletReady",
			Message:            "virtual-kubelet is posting ready status",
		},
		{
			Type:               "MemoryPressure",
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientMemory",
			Message:            "virtual-kubelet has sufficient memory available",
		},
		{
			Type:               "DiskPressure",
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasNoDiskPressure",
			Message:            "virtual-kubelet has no disk pressure",
		},
		{
			Type:               "PIDPressure",
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "KubeletHasSufficientPID",
			Message:            "virtual-kubelet has sufficient PID available",
		},
	}
}

func (p *Provider) Ping(ctx context.Context) error {
	_, span := trace.StartSpan(ctx, "Ping")
	defer span.End()
	return nil
}

func (p *Provider) NotifyNodeStatus(ctx context.Context, f func(*v1.Node)) {
	subCtx, span := trace.StartSpan(ctx, "NotifyNodeStatus")
	defer span.End()
	p.nodeHandler.notifyNodeFunc = f
	p.nodeHandler.start(subCtx)
	span.Logger().Debug("set provider node handler finish")
}
