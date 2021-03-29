package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flytam/filenamify"
	"github.com/prologic/bitcask"
	"github.com/shirou/gopsutil/process"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ProcessStore interface {
	Key() []byte
}

type ProcessManager struct {
	workdir   string
	processDb *bitcask.Bitcask
	im        *ImageManager
	record    record.EventRecorder
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

const PodPrefix = "pod://"

func NewProcessManager(workdir string, processDb *bitcask.Bitcask, im *ImageManager, record record.EventRecorder) *ProcessManager {
	return &ProcessManager{workdir: workdir, processDb: processDb, im: im, record: record}
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
	//转换pod为进程
	proc, err := m.convertPod2Process(ctx, p)
	if err != nil {
		return err
	}
	fmt.Println(proc, "=====================")
	if err := m.Put(proc); err != nil {
		return err
	}
	proc.run(ctx)
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
	fmt.Println("================开始run pod")
	go func() {
		for {
			fmt.Println("================开始run pod,当前index", p.Index)
			if p.Index == 0 {
				bus <- PodProcessStart{ProcessEvent: BaseProcessEvent{p: p, cp: nil}, t: time.Now()}
				fmt.Println("================发送PodProcessStart事件", p.Index)
			}
			if p.Index >= len(p.ProcessChain) {
				break
			}
			subCtx := context.WithValue(context.WithValue(ctx, podProcessKey, p), indexKey, p.Index)
			cp := p.ProcessChain[p.Index]
			fmt.Println("================启动ContainerProcess", cp, p.Index)
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
		if err := cp.stop(ctx); err != nil {
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

//序列化tag
type ContainerProcess struct {
	Workdir string `json:"workdir"`

	PodName   string           `json:"pod_name"`
	Namespace string           `json:"namespace"`
	Container corev1.Container `json:"container"`

	Sync          bool                 `json:"sync"`
	RestartPolicy corev1.RestartPolicy `json:"restart_policy"`

	//状态信息
	Pid           int  `json:"pid"`
	ImagePulled   bool `json:"image_pulled"`
	ImageUnzipped bool `json:"image_unzipped"`
	PidDead       bool `json:"pid_dead"`

	im *ImageManager   `json:"-"`
	pm *ProcessManager `json:"-"`
}

/**
将pod转换为进程
*/
func (m *ProcessManager) convertPod2Process(ctx context.Context, p *corev1.Pod) (proc *PodProcess, err error) {
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

func (p *ContainerProcess) run(ctx context.Context) error {
	f := func() error {
		podProc := ctx.Value(podProcessKey).(*PodProcess)
		index := ctx.Value(indexKey).(int)
		fmt.Println("pull 镜像", dockerImageName(p.Container.Image), p.ImagePulled)
		//根据镜像pull策略判断是否要pull镜像
		if p.needPullImage() {
			fmt.Println("发送 pull 镜像 事件", dockerImageName(p.Container.Image))
			bus <- BaseProcessEvent{
				p:   podProc,
				cp:  p,
				msg: fmt.Sprintf("start pull image %s", p.Container.Image),
			}
			fmt.Println("发送 pull 镜像 事件完成", dockerImageName(p.Container.Image))
			if err := p.im.PullImage(ctx, PullImageOpts{
				SrcImage: dockerImageName(p.Container.Image),
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
			//TODO:镜像已更新设置
		}
		p.ImagePulled = true
		bus <- BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: fmt.Sprintf("image %s pull finish", p.Container.Image),
		}
		//解压镜像
		fmt.Println("解压 镜像", p.Workdir)
		if !p.ImageUnzipped {
			bus <- BaseProcessEvent{
				p:   podProc,
				cp:  p,
				msg: fmt.Sprintf("start ImageUnzipped %s to workdir %s", p.Container.Image, p.Workdir),
			}
			if err := p.im.UnzipImage(ctx, p.Container.Image, p.Workdir); err != nil {
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
		//TODO:获取configmap和secret
		getConfigMaps(podProc.Pod)
		getSecrets(podProc.Pod)
		fmt.Println("运行进程", p.Container.Command, "args:", p.Container.Args)
		if p.Pid == 0 && !p.PidDead {
			//TODO:启动进程,设置环境变量
			args := append(p.Container.Command, p.Container.Args...)
			fmt.Println("=========================,run", "/bin/sh -c", strings.Join(args, " "))
			cmd := exec.Command("/bin/sh", "-c", strings.Join(args, " "))
			containerWorkDir := containerWorkDir(p.Workdir)
			cmd.Dir = filepath.Join(containerWorkDir, p.Container.WorkingDir)
			cmd.Stdin = strings.NewReader("")
			file, err := os.OpenFile(filepath.Join(containerWorkDir, "STDOUT"), os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
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
					msg: fmt.Sprintf("start cmd error image:%s,workdir:%s,cmd:%s,args:%v,err:%v", p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args, err),
				}
				return err
			} else {
				p.Pid = cmd.Process.Pid
				bus <- ContainerProcessRun{
					ProcessEvent: BaseProcessEvent{
						p:   podProc,
						cp:  p,
						msg: fmt.Sprintf("start cmd with Pid %v image:%s,workdir:%s,cmd:%s,args:%v", cmd.Process.Pid, p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args),
					},
					pid:   cmd.Process.Pid,
					index: index,
				}
				go cmd.Wait()
			}
		}
		startHealthCheck(ctx, p)
		//等待结束,并发送进程结束事件
		state, err := waitProcessDead(ctx, p)

		p.PidDead = true
		bus <- ContainerProcessFinish{
			ProcessEvent: BaseProcessEvent{
				p:   podProc,
				cp:  p,
				msg: fmt.Sprintf("run cmd end image:%s", p.Container.Image),
			},
			pid:   p.Pid,
			index: index,
			state: state,
		}
		fmt.Println("运行进程完成", p.Container.Command)
		return err
	}
	if p.Sync {
		fmt.Println("================启动ContainerProcess,同步")
		return f()
	} else {
		fmt.Println("================启动ContainerProcess,异步")
		go f()
	}
	return nil
}

func (p *ContainerProcess) needPullImage() bool {
	return !p.ImagePulled || p.Container.ImagePullPolicy == corev1.PullAlways || ImageLatest(p.Container.Image)
}

const ImageLatestSuffix = ":LATEST"

func ImageLatest(image string) bool {
	return strings.HasSuffix(strings.ToTitle(image), ImageLatestSuffix)
}

func (p *ContainerProcess) stop(ctx context.Context) error {
	proc, err := process.NewProcess(int32(p.Pid))
	if err != nil {
		return err
	}
	//TODO:优雅关闭
	return proc.KillWithContext(ctx)
}

func waitProcessDead(ctx context.Context, p *ContainerProcess) (*os.ProcessState, error) {
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

func getConfigMaps(pod *corev1.Pod) map[string]interface{} {
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

func getSecrets(pod *corev1.Pod) map[string]interface{} {
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
