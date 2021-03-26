package native

import (
	"context"
	"fmt"
	"github.com/kok-stack/native-kubelet/trace"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"strconv"
	"time"
)

func (p *Provider) GetStatsSummary(ctx context.Context) (*stats.Summary, error) {
	ctx, span := trace.StartSpan(ctx, "ConfigureNode")
	defer span.End()

	var summary stats.Summary

	//metrics, err := p.downMetricsClientSet.MetricsV1beta1().PodMetricses(v1.NamespaceAll).List(ctx, v12.ListOptions{})
	//if err != nil {
	//	return nil, err
	//}
	//var cpuAll, memoryAll uint64
	//var t time.Time
	//for _, metric := range metrics.Items {
	//	podStats := convert2PodStats(&metric)
	//	summary.Pods = append(summary.Pods, *podStats)
	//	cpuAll += *podStats.CPU.UsageNanoCores
	//	memoryAll += *podStats.Memory.WorkingSetBytes
	//	if t.IsZero() {
	//		t = podStats.StartTime.Time
	//	}
	//}
	//summary.Node = stats.NodeStats{
	//	NodeName:  p.initConfig.NodeName,
	//	StartTime: metav1.Time{Time: t},
	//	CPU: &stats.CPUStats{
	//		Time:           metav1.Time{Time: t},
	//		UsageNanoCores: &cpuAll,
	//	},
	//	Memory: &stats.MemoryStats{
	//		Time:            metav1.Time{Time: t},
	//		WorkingSetBytes: &memoryAll,
	//	},
	//}
	return &summary, nil
}

func convert2PodStats(metric *v1beta1.PodMetrics) *stats.PodStats {
	stat := &stats.PodStats{}
	if metric == nil {
		return nil
	}
	stat.PodRef.Namespace = metric.Namespace
	stat.PodRef.Name = metric.Name
	stat.StartTime = metric.Timestamp

	containerStats := stats.ContainerStats{}
	var cpuAll, memoryAll uint64
	for _, c := range metric.Containers {
		containerStats.StartTime = metric.Timestamp
		containerStats.Name = c.Name
		nanoCore := uint64(c.Usage.Cpu().ScaledValue(resource.Nano))
		memory := uint64(c.Usage.Memory().Value())
		containerStats.CPU = &stats.CPUStats{
			Time:           metric.Timestamp,
			UsageNanoCores: &nanoCore,
		}
		containerStats.Memory = &stats.MemoryStats{
			Time:            metric.Timestamp,
			WorkingSetBytes: &memory,
		}
		cpuAll += nanoCore
		memoryAll += memory
		stat.Containers = append(stat.Containers, containerStats)
	}
	stat.CPU = &stats.CPUStats{
		Time:           metric.Timestamp,
		UsageNanoCores: &cpuAll,
	}
	stat.Memory = &stats.MemoryStats{
		Time:            metric.Timestamp,
		WorkingSetBytes: &memoryAll,
	}
	return stat
}

type PodEventHandler struct {
	notifyFunc func(*v1.Pod)
	events     chan ProcessEvent
	HostIp     string
}

func (p *PodEventHandler) start(ctx context.Context) {
	AddSubscribe(p.events)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-p.events:
				fmt.Println("PodEventHandler收到消息====================")
				pod := e.getPodProcess().Pod
				switch e.(type) {
				case PodProcessStart:
					event := e.(PodProcessStart)
					p.OnPodStart(event, pod)
				case ContainerProcessError:
					event := e.(ContainerProcessError)
					p.OnError(event, pod)
				case ContainerProcessRun:
					event := e.(ContainerProcessRun)
					p.OnContainerRun(event, pod)
				case ContainerProcessFinish:
					event := e.(ContainerProcessFinish)
					p.OnFinish(event, pod)
				case ContainerProcessNext:
					event := e.(ContainerProcessNext)
					p.OnNext(event, pod)
				default:
					fmt.Println(e)
				}
				p.notifyFunc(pod)
			}
		}
	}()
}

//pod start--> [OnContainerRun-->OnError/OnFinish]-->OnNext
func (p *PodEventHandler) OnError(event ContainerProcessError, pod *v1.Pod) {
	status := pod.Status
	status.Phase = v1.PodFailed
	t := metav1.Time{time.Now()}
	status.Conditions = append(status.Conditions, v1.PodCondition{
		Type:               v1.ContainersReady,
		Status:             v1.ConditionFalse,
		LastProbeTime:      t,
		LastTransitionTime: t,
		Reason:             "ContainerProcessError",
		Message:            event.getMsg(),
	})
}

func (p *PodEventHandler) OnContainerRun(event ContainerProcessRun, pod *v1.Pod) {
	status := pod.Status
	t := metav1.Time{time.Now()}
	index := 0
	var c v1.Container
	initLen := len(pod.Spec.InitContainers)
	started := true

	if initLen < event.index {
		c = pod.Spec.InitContainers[event.index]
		status.InitContainerStatuses = append(status.InitContainerStatuses, v1.ContainerStatus{
			Name: c.Name,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			LastTerminationState: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			Ready:        false,
			RestartCount: 0,
			Image:        c.Image,
			ImageID:      c.Image,
			ContainerID:  strconv.Itoa(event.pid),
			Started:      &started,
		})
	} else {
		index = event.index - initLen
		c = pod.Spec.Containers[index]
		status.ContainerStatuses = append(status.ContainerStatuses, v1.ContainerStatus{
			Name: c.Name,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			LastTerminationState: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			Ready:        false,
			RestartCount: 0,
			Image:        c.Image,
			ImageID:      c.Image,
			ContainerID:  strconv.Itoa(event.pid),
			Started:      &started,
		})
	}
}

func (p *PodEventHandler) OnFinish(event ContainerProcessFinish, pod *v1.Pod) {
	status := pod.Status
	index := 0
	initLen := len(pod.Spec.InitContainers)
	t := metav1.Time{Time: time.Now()}

	if initLen < event.index {
		index = event.index
		containerStatus := status.InitContainerStatuses[index]
		containerStatus.State.Terminated = &v1.ContainerStateTerminated{
			//ExitCode:    0,
			//Signal:      0,
			//Reason:      "",
			//Message:     "",
			StartedAt:   containerStatus.State.Running.StartedAt,
			FinishedAt:  t,
			ContainerID: strconv.Itoa(event.pid),
		}
	} else {
		index = event.index - initLen
		containerStatus := status.ContainerStatuses[index]
		containerStatus.State.Terminated = &v1.ContainerStateTerminated{
			//ExitCode:    0,
			//Signal:      0,
			StartedAt:   containerStatus.State.Running.StartedAt,
			FinishedAt:  t,
			ContainerID: strconv.Itoa(event.pid),
		}
	}
}

func (p *PodEventHandler) OnNext(event ContainerProcessNext, pod *v1.Pod) {
	fmt.Println(event, pod)
}

func (p *PodEventHandler) OnPodStart(event PodProcessStart, pod *v1.Pod) {
	status := pod.Status
	status.Phase = v1.PodRunning
	status.HostIP = p.HostIp
	status.PodIP = p.HostIp
	status.PodIPs = []v1.PodIP{{IP: p.HostIp}}
	status.StartTime = &metav1.Time{Time: event.t}
	status.QOSClass = v1.PodQOSGuaranteed
	status.InitContainerStatuses = make([]v1.ContainerStatus, len(pod.Spec.InitContainers))
	status.ContainerStatuses = make([]v1.ContainerStatus, len(pod.Spec.Containers))
}
