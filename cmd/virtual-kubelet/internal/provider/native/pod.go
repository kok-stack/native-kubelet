package native

import (
	"context"
	"encoding/json"
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
				Printf(fmt.Sprintf("%T 更新pod.status ", e), pod.Status)
				p.notifyFunc(pod)
			}
		}
	}()
}

func Printf(s string, status v1.PodStatus) {
	marshal, err := json.Marshal(status)
	if err != nil {
		panic(err)
	}
	fmt.Println(s, "=======================")
	fmt.Println(string(marshal))
	fmt.Println(s, "=======================")
}

//pod start--> [OnContainerRun-->OnError/OnFinish]-->OnNext
func (p *PodEventHandler) OnError(event ContainerProcessError, pod *v1.Pod) {
	status := &pod.Status
	fmt.Println("===========OnError====================")
	status.Phase = v1.PodFailed
	t := metav1.Time{Time: time.Now()}
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
	status := &pod.Status
	fmt.Println("===========OnContainerRun====================")
	t := metav1.Time{Time: time.Now()}
	index := 0
	var c v1.Container
	initLen := len(pod.Spec.InitContainers)
	started := true

	if initLen < event.index {
		c = pod.Spec.InitContainers[event.index]
		status.InitContainerStatuses[event.index] = v1.ContainerStatus{
			Name: c.Name,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			LastTerminationState: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			Ready:        true,
			RestartCount: 0,
			Image:        c.Image,
			ImageID:      c.Image,
			ContainerID:  strconv.Itoa(event.pid),
			Started:      &started,
		}
	} else {
		index = event.index - initLen
		c = pod.Spec.Containers[index]
		status.ContainerStatuses[index] = v1.ContainerStatus{
			Name: c.Name,
			State: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			LastTerminationState: v1.ContainerState{
				Running: &v1.ContainerStateRunning{StartedAt: t},
			},
			Ready:        true,
			RestartCount: 0,
			Image:        c.Image,
			ImageID:      c.Image,
			ContainerID:  strconv.Itoa(event.pid),
			Started:      &started,
		}
	}
}

func (p *PodEventHandler) OnFinish(event ContainerProcessFinish, pod *v1.Pod) {
	status := &pod.Status
	fmt.Println("===========OnFinish====================")
	index := 0
	initLen := len(pod.Spec.InitContainers)
	t := metav1.Time{Time: time.Now()}
	var state *v1.ContainerState
	var lastTerminationState *v1.ContainerState

	if initLen < event.index {
		index = event.index
		state = &status.InitContainerStatuses[index].State
		lastTerminationState = &status.InitContainerStatuses[index].LastTerminationState
	} else {
		index = event.index - initLen
		state = &status.ContainerStatuses[index].State
		lastTerminationState = &status.ContainerStatuses[index].LastTerminationState
	}
	c := &v1.ContainerStateTerminated{
		ExitCode: int32(event.state.ExitCode()),
		//Signal:      0,
		//Reason:      "",
		Message:     event.getMsg(),
		StartedAt:   state.Running.StartedAt,
		FinishedAt:  t,
		ContainerID: strconv.Itoa(event.pid),
	}
	state.Terminated = c
	lastTerminationState.Terminated = c

	setPhase(pod, event.index)
}

func (p *PodEventHandler) OnNext(event ContainerProcessNext, pod *v1.Pod) {
	fmt.Println("===========OnNext====================")

	setPhase(pod, event.index)
}

/**
计算容器状态
*/
func setPhase(pod *v1.Pod, index int) {
	status := &pod.Status
	//if index == (len(pod.Status.InitContainerStatuses) + len(pod.Status.ContainerStatuses)) {
	//	return
	//}
	for _, s := range pod.Status.InitContainerStatuses {
		if s.LastTerminationState.Terminated == nil {
			continue
		}
		if s.LastTerminationState.Terminated.ExitCode != 0 {
			status.Phase = v1.PodFailed
			return
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.LastTerminationState.Terminated == nil {
			continue
		}
		if s.LastTerminationState.Terminated.ExitCode != 0 {
			status.Phase = v1.PodFailed
			return
		}
	}
	status.Phase = v1.PodSucceeded
}

func (p *PodEventHandler) OnPodStart(event PodProcessStart, pod *v1.Pod) {
	status := v1.PodStatus{}
	status.Phase = v1.PodRunning
	status.HostIP = p.HostIp
	status.PodIP = p.HostIp
	status.PodIPs = []v1.PodIP{{IP: p.HostIp}}
	status.StartTime = &metav1.Time{Time: event.t}
	status.QOSClass = v1.PodQOSGuaranteed
	status.InitContainerStatuses = make([]v1.ContainerStatus, len(pod.Spec.InitContainers))
	status.ContainerStatuses = make([]v1.ContainerStatus, len(pod.Spec.Containers))
	pod.Status = status
}
