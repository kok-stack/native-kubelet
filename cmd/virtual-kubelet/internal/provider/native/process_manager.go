package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flytam/filenamify"
	"github.com/kok-stack/native-kubelet/internal/manager"
	"github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/node/api"
	"github.com/kok-stack/native-kubelet/trace"
	"github.com/prologic/bitcask"
	"github.com/shirou/gopsutil/process"
	"io"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	ers "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/fieldpath"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	"os"
	"os/exec"
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
	go func() {
		events := make(chan ProcessEvent)
		AddSubscribe(events)

		for {
			select {
			case <-ctx.Done():
				return
			case event := <-events:
				m.recordEvent(event)

				m.Put(event.getPodProcess())
			}
		}
	}()

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
		return nil
	})
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
	subCtx, span := trace.StartSpan(ctx, "PodProcess.run")
	defer span.End()
	go func() {
		for {
			span.Logger().WithField("Index", p.Index).Debug("开始 run pod")
			if p.Index == 0 {
				bus <- PodProcessStart{ProcessEvent: BaseProcessEvent{p: p, cp: nil}, t: time.Now()}
				span.Logger().WithField("Index", p.Index).Debug("发送PodProcessStart事件完成")
			}
			if p.Index >= len(p.ProcessChain) {
				break
			}
			subCtx := context.WithValue(context.WithValue(subCtx, podProcessKey, p), indexKey, p.Index)
			cp := p.ProcessChain[p.Index]
			span.Logger().WithField("Index", p.Index).Debug("启动ContainerProcess")
			err := cp.run(subCtx)
			if err != nil {
				bus <- ContainerProcessError{ProcessEvent: BaseProcessEvent{p: p, cp: cp, err: err, msg: fmt.Sprintf("run container process error,index:%v", p.Index)}, index: p.Index}
				continue
			}
			p.Index++
			bus <- ContainerProcessNext{ProcessEvent: BaseProcessEvent{p: p, cp: cp, err: err, msg: fmt.Sprintf("run container process finish, run next ,index:%v", p.Index)}, index: p.Index}
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
		if cp.PidDead {
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
	PidDead           bool `json:"pid_dead"`
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

func (p *ContainerProcess) run(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "ContainerProcess.run")
	defer span.End()
	f := func(ctx context.Context) error {
		subCtx, span := trace.StartSpan(ctx, "ContainerProcess.run.f")
		defer span.End()
		podProc := subCtx.Value(podProcessKey).(*PodProcess)
		index := subCtx.Value(indexKey).(int)
		imageName := dockerImageName(p.Container.Image)
		subCtx = span.WithFields(subCtx, log.Fields{
			"image":               imageName,
			"p.Pid":               p.Pid,
			"p.ImagePulled":       p.ImagePulled,
			"p.ImageUnzipped":     p.ImageUnzipped,
			"p.PidDead":           p.PidDead,
			"p.ConfigMapDownload": p.ConfigMapDownload,
			"p.SecretDownload":    p.SecretDownload,
			"index":               index,
		})
		span.Logger().Debug("pull 镜像", imageName, p.ImagePulled)
		//根据镜像pull策略判断是否要pull镜像
		if p.needPullImage() {
			span.Logger().Debug("发送 pull 镜像 事件", imageName)
			bus <- BaseProcessEvent{
				p:   podProc,
				cp:  p,
				msg: fmt.Sprintf("start pull image %s", p.Container.Image),
			}
			span.Logger().Debug("发送 pull 镜像 事件完成", imageName)
			//TODO:下载镜像时,使用ImagePullSecret
			if err := p.im.PullImage(subCtx, PullImageOpts{
				SrcImage: imageName,
				//DockerAuthConfig:            nil,
				//DockerBearerRegistryToken:   "",
				//DockerRegistryUserAgent:     "",
				//DockerInsecureSkipTLSVerify: 0,
				//Timeout: time.Hour,
				//RetryCount:                  0,
			}); err != nil {
				bus <- BaseProcessEvent{
					p:   podProc,
					cp:  p,
					err: err,
					msg: fmt.Sprintf("image %s pull error", p.Container.Image),
				}
				return err
			}
		}
		p.ImagePulled = true
		bus <- BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: fmt.Sprintf("image %s pull finish", p.Container.Image),
		}
		//解压镜像
		span.Logger().Debug("开始解压镜像到workdir", p.Workdir)
		if !p.ImageUnzipped {
			bus <- BaseProcessEvent{
				p:   podProc,
				cp:  p,
				msg: fmt.Sprintf("start ImageUnzipped %s to workdir %s", p.Container.Image, p.Workdir),
			}
			if err := p.im.UnzipImage(subCtx, p.Container.Image, p.Workdir); err != nil {
				bus <- BaseProcessEvent{
					p:   podProc,
					err: err,
					cp:  p,
					msg: fmt.Sprintf("ImageUnzipped %s to workdir %s error %v", p.Container.Image, p.Workdir, err),
				}
				return err
			}
		}
		p.ImageUnzipped = true
		bus <- BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: fmt.Sprintf("ImageUnzipped %s to workdir %s finish", p.Container.Image, p.Workdir),
		}
		containerWorkDir := containerWorkDir(p.Workdir)
		if !p.ConfigMapDownload {
			if err := p.downloadConfigMaps(podProc.Pod, containerWorkDir); err != nil {
				bus <- DownloadResource{
					BaseProcessEvent{
						p:   podProc,
						err: err,
						cp:  p,
						msg: fmt.Sprintf("download configmap error image:%s,err:%v", p.Container.Image, err),
					},
				}
				return err
			} else {
				p.ConfigMapDownload = true
				bus <- DownloadResource{
					BaseProcessEvent{
						p:   podProc,
						cp:  p,
						msg: fmt.Sprintf("download configmap finish image:%s", p.Container.Image),
					},
				}
			}
		}
		if !p.SecretDownload {
			if err := p.downloadSecrets(podProc.Pod, containerWorkDir); err != nil {
				bus <- DownloadResource{
					BaseProcessEvent{
						p:   podProc,
						err: err,
						cp:  p,
						msg: fmt.Sprintf("download secret error image:%s,err:%v", p.Container.Image, err),
					},
				}
				return err
			} else {
				p.SecretDownload = true
				bus <- DownloadResource{
					BaseProcessEvent{
						p:   podProc,
						cp:  p,
						msg: fmt.Sprintf("download secret finish image:%s", p.Container.Image),
					},
				}
			}
		}
		span.Logger().Debug("开始运行进程", p.Container.Command, "args:", p.Container.Args)
		if p.Pid == 0 && !p.PidDead {
			args := append(p.Container.Command, p.Container.Args...)
			span.Logger().Debug("运行进程完整命令为", "/bin/sh -c", strings.Join(args, " "))
			cmd := exec.Command("/bin/sh", "-c", strings.Join(args, " "))
			envs, err := p.buildContainerEnvs(podProc.Pod)
			if err != nil {
				return err
			}
			cmd.Env = envs
			fmt.Println("==========workdir", filepath.Join(containerWorkDir, p.Container.WorkingDir))
			cmd.Dir = filepath.Join(containerWorkDir, p.Container.WorkingDir)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdin = strings.NewReader("")

			file, err := os.OpenFile(filepath.Join(containerWorkDir, STDOUT), os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
			if err != nil {
				return err
			}
			defer func() {
				file.Sync()
				file.Close()
			}()
			cmd.Stdout = file
			cmd.Stderr = file
			err = cmd.Start()
			if err != nil {
				bus <- BaseProcessEvent{
					p:   podProc,
					err: err,
					cp:  p,
					msg: fmt.Sprintf("start cmd error image:%s,workdir:%s,cmd:%s,args:%v,err:%v",
						p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args, err),
				}
				return err
			} else {
				p.Pid = cmd.Process.Pid
				bus <- ContainerProcessRun{
					ProcessEvent: BaseProcessEvent{
						p:  podProc,
						cp: p,
						msg: fmt.Sprintf("start cmd with Pid %v image:%s,workdir:%s,cmd:%s,args:%v",
							cmd.Process.Pid, p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args),
					},
					pid:   cmd.Process.Pid,
					index: index,
				}
				go cmd.Wait()
			}
		}
		startHealthCheck(subCtx, p)
		//等待结束,并发送进程结束事件
		state, err := waitProcessDead(p)

		p.PidDead = true
		var msg string
		if state != nil {
			msg = fmt.Sprintf("run cmd end image:%s,exitCode:%v,resion:%s", p.Container.Image, state.ExitCode(), state.String())
		} else {
			msg = fmt.Sprintf("run cmd end image:%s,err:%v", p.Container.Image, err)
		}
		bus <- ContainerProcessFinish{
			ProcessEvent: BaseProcessEvent{
				p:   podProc,
				cp:  p,
				msg: msg,
			},
			pid:   p.Pid,
			index: index,
			state: state,
		}
		span.Logger().Debug("运行进程完成", p.Container.Command)
		return err
	}
	if p.Sync {
		span.Logger().Debug("使用同步方式启动ContainerProcess")
		//TODO:根据容器重启策略判断是否重启
		return f(ctx)
	} else {
		span.Logger().Debug("使用异步方式启动ContainerProcess")
		go f(ctx)
	}
	return nil
}

func (p *ContainerProcess) buildContainerEnvs(pod *corev1.Pod) ([]string, error) {
	environ := os.Environ()
	for _, envVar := range p.Container.Env {
		if envVar.ValueFrom != nil {
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

func waitProcessDead(p *ContainerProcess) (*os.ProcessState, error) {
	proc, err := os.FindProcess(p.Pid)
	if err != nil {
		return nil, err
	}
	wait, err := proc.Wait()
	if err != nil {
		return nil, err
	}
	return wait, nil
}

//TODO:健康检查
func startHealthCheck(ctx context.Context, p *ContainerProcess) {
	go func(ctx context.Context, p *ContainerProcess) {
		for {
			//在进程存在情况下,进行健康检查,进程不存在或出现错误,则跳过
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
