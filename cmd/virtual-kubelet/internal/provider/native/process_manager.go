package native

import (
	"context"
	"encoding/json"
	"fmt"
	"git.mills.io/prologic/bitcask"
	"github.com/flytam/filenamify"
	"github.com/kok-stack/native-kubelet/internal/manager"
	"github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/node/api"
	"github.com/kok-stack/native-kubelet/trace"
	"github.com/shirou/gopsutil/process"
	"io"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	ers "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/fieldpath"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const STDOUT = "STDOUT"

type ProcessStore interface {
	Key() []byte
}

type ProcessManager struct {
	workdir         string
	processDb       *bitcask.Bitcask
	im              *ImageManager
	record          record.EventRecorder
	nodeIp          string
	resourceManager *manager.ResourceManager
}

func (m *ProcessManager) Put(store ProcessStore) error {
	marshal, err := json.Marshal(store)
	if err != nil {
		return err
	}
	err = m.processDb.Put(store.Key(), marshal)
	if err != nil {
		return err
	}
	return nil
}

func (m *ProcessManager) Delete(store ProcessStore) error {
	return m.processDb.Delete(store.Key())
}

func (m *ProcessManager) Get(key []byte) (*PodProcess, error) {
	podProcess := &PodProcess{}
	get, err := m.processDb.Get(key)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(get, podProcess); err != nil {
		return nil, err
	}
	podProcess.pm = m
	return podProcess, nil
}

const PodPrefix = "pod://"

func NewProcessManager(workdir string, processDb *bitcask.Bitcask, im *ImageManager, record record.EventRecorder, nodeIp string, manager *manager.ResourceManager) *ProcessManager {
	return &ProcessManager{workdir: workdir, processDb: processDb, im: im, record: record, nodeIp: nodeIp, resourceManager: manager}
}

var bus = make(chan ProcessEvent, 0)
var subscribes = make([]chan ProcessEvent, 0)

func AddSubscribe(ch chan ProcessEvent) {
	subscribes = append(subscribes, ch)
}

func (m *ProcessManager) Start(ctx context.Context) error {
	//分发事件
	m.startEventDispatcher(ctx)
	m.startRecorder(ctx)
	//处理restartPolicy
	m.startContainerDaemon(ctx)

	return m.processDb.Scan([]byte(PodPrefix), func(key []byte) error {
		get, err := m.processDb.Get(key)
		if err != nil {
			return err
		}
		pp := &PodProcess{}
		err = json.Unmarshal(get, pp)
		if err != nil {
			return err
		}
		pp.run(ctx)
		pp.pm = m
		return nil
	})
}

func (m *ProcessManager) startRecorder(ctx context.Context) {
	go func() {
		events := make(chan ProcessEvent)
		AddSubscribe(events)

		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				m.recordEvent(event)

				_ = m.Put(event.getPodProcess())
			}
		}
	}()
}

func (m *ProcessManager) startEventDispatcher(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-bus:
				for _, v := range subscribes {
					v <- event
				}
			}
		}
	}()
}

func (m *ProcessManager) RunPod(ctx context.Context, p *corev1.Pod) error {
	subCtx, span := trace.StartSpan(ctx, "ProcessManager.RunPod")
	defer span.End()
	//转换pod为进程
	proc, err := m.convertPod2Process(p)
	if err != nil {
		return err
	}
	if err := m.Put(proc); err != nil {
		return err
	}
	proc.run(subCtx)
	return nil
}

type PodProcess struct {
	pm        *ProcessManager `json:"-"`
	Workdir   string          `json:"workdir"`
	PodName   string          `json:"pod_name"`
	Namespace string          `json:"namespace"`

	ProcessChain []*ContainerProcess `json:"process_chain"`
	Pod          *corev1.Pod         `json:"pod"`

	Index int `json:"index"`
}

func (p *PodProcess) Key() []byte {
	return getPodKey(p.Namespace, p.PodName)
}

func (p *PodProcess) run(ctx context.Context) {
	go func() {
		subCtx, span := trace.StartSpan(ctx, "PodProcess.run")
		defer span.End()
		index := p.Index
		lenProc := len(p.ProcessChain)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if index >= lenProc {
					return
				}
				pod, err := p.pm.getPod(ctx, p.Namespace, p.PodName)
				if err != nil || ers.IsNotFound(err) {
					return
				}
				if !pod.ObjectMeta.DeletionTimestamp.IsZero() {
					return
				}

				//TODO:重启需要根据 指数退避算法 来进行重启
				span.Logger().WithField("Index", index).Debug("开始 run pod")
				if index == 0 {
					bus <- PodProcessStart{ProcessEvent: BaseProcessEvent{p: p, cp: nil}, t: time.Now()}
					span.Logger().WithField("Index", index).Debug("发送PodProcessStart事件完成")
				}
				cp := p.ProcessChain[index]
				span.Logger().WithField("Index", index).Debug("启动ContainerProcess")
				if err := cp.run(subCtx, p, index); err != nil {
					time.Sleep(time.Second * 2)
					continue
				} else {
					index++
					p.Index++
				}
			}
		}
	}()
}

/**
停止PodProcess
*/
func (p *PodProcess) stop(ctx context.Context) error {
	//找到正在运行的ContainerProcess
	cps := p.getRuningContainerProcess()
	//kill
	for _, cp := range cps {
		if err := cp.stop(ctx, *p.Pod.Spec.TerminationGracePeriodSeconds); err != nil {
			return err
		}
	}
	return nil
}

//TODO:考虑并发问题
func (p *PodProcess) getRuningContainerProcess() []*ContainerProcess {
	result := make([]*ContainerProcess, 0)
	for i, cp := range p.ProcessChain {
		if i > p.Index {
			continue
		}
		result = append(result, cp)
	}
	return result
}

func (p *PodProcess) getLog(ctx context.Context, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "PodProcess.getLog")
	defer span.End()
	c := p.getContainerProcess(containerName)
	if c == nil {
		return nil, fmt.Errorf("namespace %s pod %s 中 container %s 未找到", p.Namespace, p.PodName, containerName)
	}
	return c.getLog(ctx, opts)
}

func (p *PodProcess) getContainerProcess(name string) *ContainerProcess {
	for _, c := range p.ProcessChain {
		if c.Container.Name == name {
			return c
		}
	}
	return nil
}

//序列化tag
type ContainerProcess struct {
	Workdir string `json:"workdir"`

	PodName   string           `json:"pod_name"`
	Namespace string           `json:"namespace"`
	Container corev1.Container `json:"container"`

	Sync          bool                 `json:"sync"`
	RestartPolicy corev1.RestartPolicy `json:"restart_policy"`

	//状态信息
	Pid               int  `json:"pid"`
	ImagePulled       bool `json:"image_pulled"`
	ImageUnzipped     bool `json:"image_unzipped"`
	ConfigMapDownload bool `json:"config_map_download"`
	SecretDownload    bool `json:"secret_download"`

	im *ImageManager   `json:"-"`
	pm *ProcessManager `json:"-"`
}

/**
将pod转换为进程
*/
func (m *ProcessManager) convertPod2Process(p *corev1.Pod) (proc *PodProcess, err error) {
	dir, err := getProcessWorkDir(m.workdir, p)
	if err != nil {
		return nil, err
	}
	proc = &PodProcess{
		Workdir:   dir,
		PodName:   p.Name,
		Namespace: p.Namespace,
		Pod:       p,
		pm:        m,
	}
	initLen := len(p.Spec.InitContainers)
	proc.ProcessChain = make([]*ContainerProcess, initLen+len(p.Spec.Containers))
	for i, c := range p.Spec.InitContainers {
		dir, err = getContainerProcessWorkDir(m.workdir, p, c)
		if err != nil {
			return nil, err
		}
		proc.ProcessChain[i] = NewProcess(p, c, dir, m.im, m, true)
	}
	for i, c := range p.Spec.Containers {
		dir, err = getContainerProcessWorkDir(m.workdir, p, c)
		if err != nil {
			return nil, err
		}
		proc.ProcessChain[i+initLen] = NewProcess(p, c, dir, m.im, m, false)
	}
	return proc, nil
}

func (m *ProcessManager) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	origin, err := m.processDb.Get(getPodKey(pod.Namespace, pod.Name))
	if err != nil {
		if err == bitcask.ErrKeyNotFound {
			return nil
		}
		return err
	}

	p := &PodProcess{}
	if err = json.Unmarshal(origin, p); err != nil {
		return err
	}
	if err = p.stop(ctx); err != nil {
		return err
	}

	return m.Delete(p)
}

func getPodKey(ns, podName string) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", PodPrefix, ns, podName))
}

func (m *ProcessManager) getPod(ctx context.Context, namespace string, name string) (*corev1.Pod, error) {
	get, err := m.processDb.Get(getPodKey(namespace, name))
	if err != nil {
		if err == bitcask.ErrKeyNotFound {
			return nil, nil
		}
		return nil, err
	}
	p := &PodProcess{}
	err = json.Unmarshal(get, p)
	if err != nil {
		return nil, err
	}
	p.pm = m
	return p.Pod, nil
}

func (m *ProcessManager) getPods(ctx context.Context) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0)
	err := m.processDb.Scan([]byte(PodPrefix), func(key []byte) error {
		get, err := m.processDb.Get(key)
		if err != nil {
			return err
		}
		p := &PodProcess{}
		err = json.Unmarshal(get, p)
		if err != nil {
			return err
		}
		pods = append(pods, p.Pod)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func (m *ProcessManager) recordEvent(event ProcessEvent) {
	var eventtype string
	if event.getError() != nil {
		eventtype = corev1.EventTypeWarning
	} else {
		eventtype = corev1.EventTypeNormal
	}
	m.record.Event(event.getPodProcess().Pod, eventtype, "ProcessEvent", event.getMsg())
}

func (m *ProcessManager) getPodLog(ctx context.Context, namespace string, podName string, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "ProcessManager.getPodLog")
	defer span.End()
	key := getPodKey(namespace, podName)
	get, err := m.Get(key)
	if err != nil {
		return nil, err
	}
	return get.getLog(ctx, containerName, opts)
}

func (m *ProcessManager) startContainerDaemon(ctx context.Context) {
	go func() {
		events := make(chan ProcessEvent)
		AddSubscribe(events)
		_, span := trace.StartSpan(ctx, "ProcessManager.startContainerDaemon")
		defer span.End()

		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				containerProcessFinish, ok := event.(ContainerProcessFinish)
				if !ok {
					continue
				}
				p := containerProcessFinish.getPodProcess()
				cp := containerProcessFinish.getContainerProcess()
				logger := span.Logger().WithFields(log.Fields{
					"namespace": p.Namespace,
					"pod":       p.PodName,
					"container": cp.Container.Name,
				})
				logger.Debug("获取到containerProcessFinish事件")
				if cp.Sync {
					logger.Debug("init容器,不执行重启")
					continue
				}
				pod, err := p.pm.getPod(ctx, p.Namespace, p.PodName)
				if err != nil || ers.IsNotFound(err) || pod == nil {
					logger.Debug("pod未获取到,不执行重启")
					continue
				}
				if pod.ObjectMeta.DeletionTimestamp != nil && !pod.ObjectMeta.DeletionTimestamp.IsZero() {
					logger.Debug("pod设置了删除时间,不执行重启")
					continue
				}
				getPod, err := p.pm.resourceManager.GetPod(p.Namespace, p.PodName)
				if err != nil {
					if ers.IsNotFound(err) {
						logger.Debugf("podLister中未获取到pod %s/%s,不执行重启", p.Namespace, p.PodName)
					} else {
						logger.Debugf("获取pod错误,err:%v,不执行重启", err)
					}
					continue
				}
				if pod.UID != getPod.UID {
					logger.Warnf("pod %s/%s 本地UID与apiserver不相等,本地:%s,server:%s,不执行重启", p.Namespace, p.PodName, pod.UID, getPod.UID)
					continue
				}

				switch p.Pod.Spec.RestartPolicy {
				case corev1.RestartPolicyAlways:
				case corev1.RestartPolicyOnFailure:
					if containerProcessFinish.getError() == nil && (containerProcessFinish.state != nil && containerProcessFinish.state.Success()) {
						logger.Debug("RestartPolicyOnFailure 并且退出码为0,不执行重启")
						continue
					}
				case corev1.RestartPolicyNever:
					logger.Debug("RestartPolicyNever,不执行重启")
					continue
				}
				_ = cp.run(ctx, p, containerProcessFinish.index)
				logger.Debug("已重启container")
			}
		}
	}()
}

func (p *ContainerProcess) run(ctx context.Context, podProc *PodProcess, index int) error {
	subCtx, span := trace.StartSpan(ctx, "ContainerProcess.run")
	defer span.End()
	imageName := dockerImageName(p.Container.Image)
	subCtx = span.WithFields(subCtx, log.Fields{
		"image":               imageName,
		"p.Pid":               p.Pid,
		"p.ImagePulled":       p.ImagePulled,
		"p.ImageUnzipped":     p.ImageUnzipped,
		"p.ConfigMapDownload": p.ConfigMapDownload,
		"p.SecretDownload":    p.SecretDownload,
		"index":               index,
	})
	f := func(ctx context.Context) error {
		if err := pull(subCtx, span, p, bus, podProc, imageName); err != nil {
			return err
		}
		//解压镜像
		if err := unzip(subCtx, span, p, bus, podProc); err != nil {
			return err
		}

		containerWorkDir := containerWorkDir(p.Workdir)
		if err := downloadResource(p, podProc, containerWorkDir); err != nil {
			return err
		}
		runSignal := make(chan error)
		go runProcess(span, p, podProc, containerWorkDir, index, runSignal)
		select {
		case <-ctx.Done():
			return nil
		case err := <-runSignal:
			if err != nil {
				return err
			}
		}
		startHealthCheck(subCtx, p)
		//等待结束,并发送进程结束事件
		processWaiter(p, podProc, index, span)
		return nil
	}
	startFunc := func() error {
		if err := f(ctx); err != nil {
			bus <- ContainerProcessFinish{
				ProcessEvent: BaseProcessEvent{
					p:   podProc,
					cp:  p,
					err: err,
					msg: fmt.Sprintf("run container process error,index:%v", index),
				},
				index: index,
				pid:   -1,
			}
			return err
		}
		return nil
	}
	if p.Sync {
		span.Logger().Debug("使用同步方式启动ContainerProcess")
		return startFunc()
	} else {
		span.Logger().Debug("使用异步方式启动ContainerProcess")
		go startFunc()
		return nil
	}
}

func (p *ContainerProcess) buildContainerEnvs(pod *corev1.Pod) ([]string, error) {
	environ := os.Environ()
	for _, envVar := range p.Container.Env {
		if envVar.ValueFrom != nil {
			//在internal\podutils\env.go中处理了ValueFrom,此处不会再存在ValueFrom不为空的情况
			val, err := p.buildEvnEntry(envVar, pod)
			if err != nil {
				return nil, err
			}
			environ = append(environ, formatEnv(envVar.Name, val))
		} else {
			environ = append(environ, formatEnv(envVar.Name, envVar.Value))
		}
	}
	return environ, nil
}

func (p *ContainerProcess) buildEvnEntry(envVar corev1.EnvVar, pod *corev1.Pod) (string, error) {
	if envVar.ValueFrom.FieldRef != nil {
		path := envVar.ValueFrom.FieldRef.FieldPath
		switch path {
		case "status.phase",
			"status.hostIP",
			"status.podIP",
			"status.podIPs",
			"spec.host":
			return p.pm.nodeIp, nil
		}
		asString, err := fieldpath.ExtractFieldPathAsString(pod, path)
		if err != nil {
			return "", err
		}
		return asString, nil
	}
	namespace := pod.Namespace
	if envVar.ValueFrom.ConfigMapKeyRef != nil {
		name := envVar.ValueFrom.ConfigMapKeyRef.Name
		cm, err := p.pm.resourceManager.GetConfigMap(name, namespace)
		if err != nil {
			if ers.IsNotFound(err) && *envVar.ValueFrom.ConfigMapKeyRef.Optional {
				return "", nil
			}
			return "", err
		}
		key := envVar.ValueFrom.ConfigMapKeyRef.Key
		s, ok := cm.Data[key]
		if !ok {
			if *envVar.ValueFrom.ConfigMapKeyRef.Optional {
				return "", nil
			}
			return "", fmt.Errorf("not found configmap %s key %s at namespace %s", name, key, namespace)
		}
		return s, nil
	}
	if envVar.ValueFrom.SecretKeyRef != nil {
		name := envVar.ValueFrom.SecretKeyRef.Name
		sec, err := p.pm.resourceManager.GetSecret(name, namespace)
		if err != nil {
			if ers.IsNotFound(err) && *envVar.ValueFrom.SecretKeyRef.Optional {
				return "", nil
			}
			return "", err
		}
		key := envVar.ValueFrom.SecretKeyRef.Key
		s, ok := sec.StringData[key]
		if !ok {
			if *envVar.ValueFrom.SecretKeyRef.Optional {
				return "", nil
			}
			return "", fmt.Errorf("not found secret %s key %s at namespace %s", name, key, namespace)
		}
		return s, nil
	}
	if envVar.ValueFrom.ResourceFieldRef != nil {
		//unsupport resource limit
		return "", fmt.Errorf("unsupport ResourceFieldRef")
	}
	return "", nil
}

func formatEnv(name, value string) string {
	return fmt.Sprintf("%s=%s", name, value)
}

func (p *ContainerProcess) needPullImage() bool {
	return !p.ImagePulled || p.Container.ImagePullPolicy == corev1.PullAlways || ImageLatest(p.Container.Image)
}

const ImageLatestSuffix = ":LATEST"

func ImageLatest(image string) bool {
	return strings.HasSuffix(strings.ToTitle(image), ImageLatestSuffix)
}

/*
立即发送SIGTERM
根据pod的terminationGracePeriodSeconds时间,发送kill
*/
func (p *ContainerProcess) stop(ctx context.Context, terminationSeconds int64) error {
	proc, err := os.FindProcess(p.Pid)
	if err != nil {
		return err
	}
	const alreadyErr = "os: process already released"
	const notInitErr = "os: process not initialized"
	const finishedErr = "os: process already finished"
	if err = proc.Signal(syscall.SIGTERM); err != nil {
		if err.Error() == alreadyErr || err.Error() == notInitErr || err.Error() == finishedErr {
			return nil
		}
		return err
	}
	timeout, cancelFunc := context.WithTimeout(ctx, time.Duration(terminationSeconds))
	defer cancelFunc()

	killer := func() error {
		if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
			if err.Error() == alreadyErr || err.Error() == notInitErr || err.Error() == finishedErr {
				return nil
			}
			return err
		}
		return nil
	}

	select {
	case <-ctx.Done():
		return killer()
	case <-timeout.Done():
		return killer()
	}
}

//TODO:健康检查
func startHealthCheck(ctx context.Context, p *ContainerProcess) {
	go func(ctx context.Context, p *ContainerProcess) {
		for {
			//在进程存在情况下,进行健康检查,进程不存在或出现错误,则不执行重启
			exists, err := process.PidExists(int32(p.Pid))
			if err != nil {
				panic(err)
			}
			if !exists {
				return
			}
			select {
			case <-ctx.Done():
				return
				//livenessProbe,readinessProbe
			}
		}
	}(ctx, p)
}

func NewProcess(pod *corev1.Pod, c corev1.Container, workdir string, im *ImageManager, pm *ProcessManager, sync bool) *ContainerProcess {
	return &ContainerProcess{
		Workdir:       workdir,
		PodName:       pod.Name,
		Namespace:     pod.Namespace,
		Container:     c,
		RestartPolicy: pod.Spec.RestartPolicy,
		im:            im,
		pm:            pm,
		Sync:          sync,
	}
}

func getProcessWorkDir(workdir string, pod *corev1.Pod) (string, error) {
	ns, err := filenamify.Filenamify(pod.Namespace, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	name, err := filenamify.Filenamify(pod.Name, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}

	join := filepath.Join(workdir, ns, name)
	return join, nil
}

func getContainerProcessWorkDir(workdir string, pod *corev1.Pod, c corev1.Container) (string, error) {
	dir, err := getProcessWorkDir(workdir, pod)
	if err != nil {
		return "", err
	}

	s, err := filenamify.Filenamify(c.Name, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, s), nil
}

func dockerImageName(image string) string {
	return dockershim.DockerImageIDPrefix + image
}

func (p *ContainerProcess) downloadConfigMaps(pod *corev1.Pod, workdir string) error {
	namespace := pod.Namespace
	for _, volume := range pod.Spec.Volumes {
		if volume.ConfigMap != nil {
			name := volume.ConfigMap.Name
			cm, err := p.pm.resourceManager.GetConfigMap(name, namespace)
			if err != nil {
				if ers.IsNotFound(err) && *volume.ConfigMap.Optional {
					continue
				}
				return err
			}
			var paths []corev1.KeyToPath
			if len(volume.ConfigMap.Items) == 0 {
				paths = make([]corev1.KeyToPath, 0, len(cm.Data))
				for k := range cm.Data {
					paths = append(paths, corev1.KeyToPath{
						Key:  k,
						Path: k,
					})
				}
			} else {
				paths = volume.ConfigMap.Items
			}
			for _, item := range paths {
				bytes, ok := cm.Data[item.Key]
				if !ok {
					if volume.ConfigMap.Optional != nil && *(volume.ConfigMap.Optional) {
						continue
					} else {
						return fmt.Errorf("not found volume configmap %s key %s in namespace %s", name, item.Key, namespace)
					}
				}
				mountPath := item.Path
				for _, mount := range p.Container.VolumeMounts {
					if mount.Name == volume.Name && mount.SubPath == item.Path {
						mountPath = mount.MountPath
						break
					}
				}

				err := ioutil.WriteFile(filepath.Join(workdir, mountPath), []byte(bytes), os.ModePerm)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (p *ContainerProcess) downloadSecrets(pod *corev1.Pod, workdir string) error {
	namespace := pod.Namespace
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			name := volume.Secret.SecretName
			sec, err := p.pm.resourceManager.GetSecret(name, namespace)
			if err != nil {
				if ers.IsNotFound(err) && *volume.Secret.Optional {
					continue
				}
				return err
			}
			var paths []corev1.KeyToPath
			if len(volume.Secret.Items) == 0 {
				paths = make([]corev1.KeyToPath, 0, len(sec.Data))
				for k := range sec.Data {
					paths = append(paths, corev1.KeyToPath{
						Key:  k,
						Path: k,
					})
				}
			} else {
				paths = volume.Secret.Items
			}
			for _, item := range paths {
				s, ok := sec.Data[item.Key]
				if !ok {
					if volume.Secret.Optional != nil && *volume.Secret.Optional {
						continue
					} else {
						return fmt.Errorf("not found volume secret %s key %s in namespace %s", name, item.Key, namespace)
					}
				}
				mountPath := item.Path
				for _, mount := range p.Container.VolumeMounts {
					if mount.Name == volume.Name && mount.SubPath == item.Path {
						mountPath = mount.MountPath
						break
					}
				}

				err := ioutil.WriteFile(filepath.Join(workdir, mountPath), s, os.ModePerm)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *ContainerProcess) getLog(ctx context.Context, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "ContainerProcess.getLog")
	defer span.End()
	//TODO:opts参数使用
	dir := containerWorkDir(p.Workdir)
	join := filepath.Join(dir, STDOUT)
	return os.OpenFile(join, os.O_RDONLY, os.ModePerm)
}
