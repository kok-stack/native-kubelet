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
	//TODO:实现计算内存,CPU等操作
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
	go func() {
		AddSubscribe(p.events)
		_, span := trace.StartSpan(ctx, "PodEventHandler.start")
		defer span.End()
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
				case ContainerProcessRun:
					event := e.(ContainerProcessRun)
					p.OnContainerRun(event, pod)
				case ContainerProcessFinish:
					event := e.(ContainerProcessFinish)
					p.OnFinish(event, pod)
				case DownloadResource:
					event := e.(DownloadResource)
					p.OnDownloadResource(event, pod)
				default:
				}
				p.notifyFunc(pod)
				//marshal, _ := json.Marshal(pod.Status)
				//fmt.Println("目前pod.status状态:", string(marshal))
			}
		}
	}()
}

//pod start--> [OnContainerRun-->OnFinish]-->OnNext

func (p *PodEventHandler) OnContainerRun(event ContainerProcessRun, pod *v1.Pod) {
	status := pod.Status
	fmt.Println("===========OnContainerRun====================")
	t := metav1.Time{Time: time.Now()}
	initLen := len(pod.Spec.InitContainers)
	started := true

	if initLen < event.index {
		containerStatus := status.InitContainerStatuses[event.index]
		containerStatus.RestartCount = containerStatus.RestartCount + 1
		containerStatus.State = v1.ContainerState{
			Running: &v1.ContainerStateRunning{StartedAt: t},
		}
		containerStatus.LastTerminationState = v1.ContainerState{
			Running: &v1.ContainerStateRunning{StartedAt: t},
		}
		containerStatus.Ready = true
		containerStatus.ContainerID = strconv.Itoa(event.pid)
		containerStatus.Started = &started
		status.InitContainerStatuses[event.index] = containerStatus
	} else {
		index := event.index - initLen
		containerStatus := status.ContainerStatuses[index]
		containerStatus.RestartCount = containerStatus.RestartCount + 1
		containerStatus.State = v1.ContainerState{
			Running: &v1.ContainerStateRunning{StartedAt: t},
		}
		containerStatus.LastTerminationState = v1.ContainerState{
			Running: &v1.ContainerStateRunning{StartedAt: t},
		}
		containerStatus.Ready = true
		containerStatus.ContainerID = strconv.Itoa(event.pid)
		containerStatus.Started = &started
		status.ContainerStatuses[event.index] = containerStatus
	}
	pod.Status = status
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
	exitCode := -1
	if event.state != nil {
		exitCode = event.state.ExitCode()
	}
	var startAt metav1.Time
	if state.Running != nil {
		startAt = state.Running.StartedAt
	}
	c := &v1.ContainerStateTerminated{
		ExitCode: int32(exitCode),
		//Signal:      0,
		//Reason:      "",
		Message:     event.getMsg(),
		StartedAt:   startAt,
		FinishedAt:  t,
		ContainerID: strconv.Itoa(event.pid),
	}
	state.Terminated = c

	lastTerminationState.Terminated = c

	setPhase(pod, exitCode)
}

/**
计算容器状态
*/
func setPhase(pod *v1.Pod, exitCode int) {
	status := &pod.Status
	if exitCode != 0 {
		return
	}
	for _, s := range pod.Status.InitContainerStatuses {
		if s.LastTerminationState.Terminated == nil {
			continue
		}
		if s.LastTerminationState.Terminated.ExitCode != 0 {
			return
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.LastTerminationState.Terminated == nil {
			continue
		}
		if s.LastTerminationState.Terminated.ExitCode != 0 {
			return
		}
	}
	status.Phase = v1.PodSucceeded
}

func (p *PodEventHandler) OnPodStart(event PodProcessStart, pod *v1.Pod) {
	fmt.Println("====================OnPodStart========================")
	status := v1.PodStatus{}
	status.Phase = v1.PodRunning
	status.HostIP = p.HostIp
	status.PodIP = p.HostIp
	status.PodIPs = []v1.PodIP{{IP: p.HostIp}}
	status.StartTime = &metav1.Time{Time: event.t}
	status.QOSClass = v1.PodQOSGuaranteed
	containerLens := len(pod.Spec.InitContainers)
	initStatuses := make([]v1.ContainerStatus, 0, containerLens)
	started := false
	for i := 0; i < containerLens; i++ {
		c := pod.Spec.InitContainers[i]
		initStatuses = append(initStatuses, v1.ContainerStatus{
			Name:                 c.Name,
			State:                v1.ContainerState{},
			LastTerminationState: v1.ContainerState{},
			Ready:                false,
			RestartCount:         -1,
			Image:                c.Image,
			ImageID:              c.Image,
			ContainerID:          "",
			Started:              &started,
		})
	}
	status.InitContainerStatuses = initStatuses
	containerLens = len(pod.Spec.Containers)
	statuses := make([]v1.ContainerStatus, 0, containerLens)
	for i := 0; i < containerLens; i++ {
		c := pod.Spec.Containers[i]
		statuses = append(statuses, v1.ContainerStatus{
			Name:                 c.Name,
			State:                v1.ContainerState{},
			LastTerminationState: v1.ContainerState{},
			Ready:                false,
			RestartCount:         -1,
			Image:                c.Image,
			ImageID:              c.Image,
			ContainerID:          "",
			Started:              &started,
		})
	}
	status.ContainerStatuses = statuses

	pod.Status = status
}

func (p *PodEventHandler) OnDownloadResource(event DownloadResource, pod *v1.Pod) {
	status := &(pod.Status)
	if event.err != nil {
		status.Phase = v1.PodFailed
	}
}
