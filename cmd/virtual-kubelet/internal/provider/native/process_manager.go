package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flytam/filenamify"
	"github.com/prologic/bitcask"
	"github.com/shirou/gopsutil/process"
	corev1 "k8s.io/api/core/v1"
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

func NewProcessManager(workdir string, processDb *bitcask.Bitcask, im *ImageManager) *ProcessManager {
	return &ProcessManager{workdir: workdir, processDb: processDb, im: im}
}

func (m *ProcessManager) Start(ctx context.Context) error {
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

	//根据存储,查找那些进程在运行,那些没有运行
	//err := m.processDb.Scan([]byte(ProcessPrefix), func(key []byte) error {
	//	get, err := m.processDb.Get(key)
	//	if err != nil {
	//		return err
	//	}
	//	cp := &ContainerProcess{}
	//	err = json.Unmarshal(get, cp)
	//	if err != nil {
	//		return err
	//	}
	//	cp.im = m.im
	//	cp.pm = m
	//	pid := int32(cp.Pid)
	//	exists, err := process.PidExists(pid)
	//	if err != nil {
	//		return err
	//	}
	//	if exists {
	//		//重启健康检查
	//		startHealthCheck(ctx, cp, cp.Pid)
	//	} else {
	//		//重新运行
	//		go cp.run(ctx)
	//	}
	//	return nil
	//})
	//if err != nil {
	//	return err
	//}
	//
	//return nil
}

func (m *ProcessManager) RunPod(ctx context.Context, p *corev1.Pod) error {
	//转换pod为进程
	proc, err := m.convertPod2Process(ctx, p)
	if err != nil {
		return err
	}
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

//如何记录index?如何更新pod?eventbus?
func (p *PodProcess) run(ctx context.Context) {
	go func() {
		for {
			if p.Index >= len(p.ProcessChain) {
				break
			}
			cp := p.ProcessChain[p.Index]
			err := cp.run(ctx)
			if err != nil {
				//TODO:record
				continue
			}
			p.Index++
			if err := p.pm.Put(p); err != nil {
				panic(err)
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
		if cp.Dead {
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
	Pid                int  `json:"pid"`
	PullImage          bool `json:"pull_image"`
	DecompressionImage bool `json:"decompression_image"`
	Dead               bool `json:"dead"`

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
	proc.ProcessChain = make([]*ContainerProcess, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for _, c := range p.Spec.InitContainers {
		dir, err = getContainerProcessWorkDir(m.workdir, p, c)
		if err != nil {
			return nil, err
		}
		proc.ProcessChain = append(proc.ProcessChain, NewProcess(p, c, dir, m.im, m, false))
	}
	for _, c := range p.Spec.Containers {
		dir, err = getContainerProcessWorkDir(m.workdir, p, c)
		if err != nil {
			return nil, err
		}
		proc.ProcessChain = append(proc.ProcessChain, NewProcess(p, c, dir, m.im, m, true))
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

func (p *ContainerProcess) run(ctx context.Context) error {
	f := func() error {
		//TODO:检查镜像是否存在,如果不存在则pull
		if p.PullImage {
			if err := p.im.PullImage(ctx, PullImageOpts{
				SrcImage: dockerImageName(p.Container.Image),
				//DockerAuthConfig:            nil,
				//DockerBearerRegistryToken:   "",
				//DockerRegistryUserAgent:     "",
				//DockerInsecureSkipTLSVerify: 0,
				//Timeout:                     0,
				//RetryCount:                  0,
			}); err != nil {
				return err
			}
		}
		//解压镜像
		if p.DecompressionImage {
			if err := p.im.decompressionImage(ctx, p.Container.Image, p.Workdir); err != nil {
				return err
			}
		}
		//TODO:获取configmap和secret

		if p.Pid == 0 && !p.Dead {
			//TODO:启动进程,设置环境变量
			cmd := exec.Command(strings.Join(p.Container.Command, " "), p.Container.Args...)
			cmd.Dir = filepath.Join(p.Workdir, p.Container.WorkingDir)
			cmd.Stdin = strings.NewReader("")
			//TODO:设置输出
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err := cmd.Start()
			if err != nil {
				return err
			}

			//TODO:产生process启动的事件(PID)
			//存储进程信息
			//p.Pid = cmd.Process.Pid
			//if err := p.pm.Put(p); err != nil {
			//	return err
			//}
			//defer func() {
			//	if err := p.pm.Delete(p); err != nil {
			//		panic(err)
			//	}
			//}()
			//return cmd.Wait()
		}
		startHealthCheck(ctx, p)
		//等待结束,并发送进程结束事件
		waitProcessDead(ctx, p)
		//TODO:发送结束事件
		return nil
	}
	if p.Sync {
		return f()
	} else {
		go f()
	}
	return nil
}

func (p *ContainerProcess) stop(ctx context.Context) error {
	proc, err := process.NewProcess(int32(p.Pid))
	if err != nil {
		return err
	}
	//TODO:优雅关闭
	return proc.KillWithContext(ctx)
}

func waitProcessDead(ctx context.Context, p *ContainerProcess) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			exists, err := process.PidExists(int32(p.Pid))
			if err != nil {
				panic(err)
			}
			if !exists {
				return
			}
		}
	}
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
	join := filepath.Join(workdir, pod.Namespace, pod.Name)
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}

func getContainerProcessWorkDir(workdir string, pod *corev1.Pod, c corev1.Container) (string, error) {
	dir, err := getProcessWorkDir(workdir, pod)
	if err != nil {
		return "", err
	}

	join := filepath.Join(dir, c.Name)
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}

func dockerImageName(image string) string {
	return dockershim.DockerImageIDPrefix + image
}
