package native

import (
	"context"
	"github.com/kok-stack/native-kubelet/trace"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"strings"
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

func getConfigMaps(pod *v1.Pod) map[string]interface{} {
	m := make(map[string]interface{})
	for _, volume := range pod.Spec.Volumes {
		if volume.ConfigMap != nil {
			m[volume.ConfigMap.Name] = nil
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, envVar := range c.Env {
			if envVar.ValueFrom != nil && envVar.ValueFrom.ConfigMapKeyRef != nil {
				m[envVar.ValueFrom.ConfigMapKeyRef.Name] = nil
			}
		}
		for _, s := range c.EnvFrom {
			if s.ConfigMapRef != nil {
				m[s.ConfigMapRef.Name] = nil
			}
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, envVar := range c.Env {
			if envVar.ValueFrom != nil && envVar.ValueFrom.ConfigMapKeyRef != nil {
				m[envVar.ValueFrom.ConfigMapKeyRef.Name] = nil
			}
		}
		for _, s := range c.EnvFrom {
			if s.ConfigMapRef != nil {
				m[s.ConfigMapRef.Name] = nil
			}
		}
	}
	return m
}

func getSecrets(pod *v1.Pod) map[string]interface{} {
	m := make(map[string]interface{})
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			m[volume.Secret.SecretName] = nil
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, envVar := range c.Env {
			if envVar.ValueFrom != nil && envVar.ValueFrom.SecretKeyRef != nil {
				m[envVar.ValueFrom.SecretKeyRef.Name] = nil
			}
		}
		for _, s := range c.EnvFrom {
			if s.SecretRef != nil {
				m[s.SecretRef.Name] = nil
			}
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, envVar := range c.Env {
			if envVar.ValueFrom != nil && envVar.ValueFrom.SecretKeyRef != nil {
				m[envVar.ValueFrom.SecretKeyRef.Name] = nil
			}
		}
		for _, s := range c.EnvFrom {
			if s.SecretRef != nil {
				m[s.SecretRef.Name] = nil
			}
		}
	}
	if pod.Spec.ImagePullSecrets != nil {
		for _, s := range pod.Spec.ImagePullSecrets {
			m[s.Name] = nil
		}
	}
	return m
}

type PodEventHandler struct {
	ctx          context.Context
	p            *Provider
	notifyFunc   func(*v1.Pod)
	podUpdateCh  chan *v1.Pod
	nodeUpdateCh chan struct{}
	nodeHandler  *NodeEventHandler
}

func (p *PodEventHandler) OnAdd(obj interface{}) {
	_, span := trace.StartSpan(p.ctx, "PodEventHandler.OnAdd")
	defer span.End()
	downPod := obj.(*v1.Pod)
	upPod, err := p.p.initConfig.ResourceManager.GetPod(downPod.Namespace, downPod.Name)
	if err != nil {
		println(err.Error())
		return
	}
	if upPod == nil {
		return
	}
	upPod = upPod.DeepCopy()
	upPod.Spec = downPod.Spec
	upPod.Status = downPod.Status
	p.podUpdateCh <- upPod
	p.nodeHandler.OnPodAdd(upPod)
}

func (p *PodEventHandler) OnUpdate(oldObj, newObj interface{}) {
	_, span := trace.StartSpan(p.ctx, "PodEventHandler.OnUpdate")
	defer span.End()
	p.OnAdd(newObj)
	oldPod := oldObj.(*v1.Pod)
	p.nodeHandler.OnPodDelete(oldPod)
}

func (p *PodEventHandler) OnDelete(obj interface{}) {
	_, span := trace.StartSpan(p.ctx, "PodEventHandler.OnDelete")
	defer span.End()
	downPod := obj.(*v1.Pod)
	//err := p.p.initConfig.ResourceManager.DeletePod(p.ctx, downPod.GetNamespace(), downPod.GetName())
	//if (err != nil && errors2.IsNotFound(err)) || err == nil {
	//	return
	//} else {
	//	println(err.Error())
	//}
	p.nodeHandler.OnPodDelete(downPod)
}

func (p *PodEventHandler) start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				close(p.podUpdateCh)
				return
			case t := <-p.podUpdateCh:
				p.notifyFunc(t)
			}
		}
	}()
}

func isDefaultSecret(secretName string) bool {
	return strings.HasPrefix(secretName, defaultTokenNamePrefix)
}

const defaultTokenNamePrefix = "default-token"
