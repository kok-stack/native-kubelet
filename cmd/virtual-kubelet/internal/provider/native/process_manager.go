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
)

type ProcessManager struct {
	workdir   string
	processDb *bitcask.Bitcask
	im        *ImageManager
}

const ProcessPrefix = "process://"
const PodPrefix = "pod://"

func NewProcessManager(workdir string, processDb *bitcask.Bitcask, im *ImageManager) *ProcessManager {
	return &ProcessManager{workdir: workdir, processDb: processDb, im: im}
}

func (m *ProcessManager) Start(ctx context.Context) error {
	//根据存储,查找那些进程在运行,那些没有运行
	err := m.processDb.Scan([]byte(ProcessPrefix), func(key []byte) error {
		get, err := m.processDb.Get(key)
		if err != nil {
			return err
		}
		cp := &ContainerProcess{}
		err = json.Unmarshal(get, cp)
		if err != nil {
			return err
		}
		cp.im = m.im
		cp.pm = m
		pid := int32(cp.Pid)
		exists, err := process.PidExists(pid)
		if err != nil {
			return err
		}
		if exists {
			//重启健康检查
			startHealthCheck(ctx, cp, cp.Pid)
		} else {
			//重新运行
			go cp.run(ctx)
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (m *ProcessManager) RunPod(ctx context.Context, p *corev1.Pod) error {
	//转换pod为进程
	proc, err := m.convertPod2Process(ctx, p)
	if err != nil {
		//TODO: 转换pod出现错误,将错误使用record记录到pod中
	}
	marshal, err := json.Marshal(proc)
	if err != nil {
		return err
	}
	err = m.processDb.Put(getPodKey(p.Namespace, p.Name), marshal)
	if err != nil {
		return err
	}
	return proc.run(ctx)
}

type PodProcess struct {
	Workdir   string `json:"workdir"`
	PodName   string `json:"pod_name"`
	Namespace string `json:"namespace"`

	ProcessChain []*ContainerProcess `json:"process_chain"`
	Pod          *corev1.Pod         `json:"pod"`

	//Index        int                 `json:"index"`
}

//如何记录index?如何更新pod?eventbus?
func (p *PodProcess) run(ctx context.Context) error {
	for _, proc := range p.ProcessChain {
		err := proc.run(ctx)
		if err != nil {
			return err
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

	Pid int `json:"pid"`

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

func (m *ProcessManager) delete(ctx context.Context, pod *corev1.Pod) error {
	for _, c := range pod.Spec.InitContainers {
		if err := m.processDb.Delete(getProcessKey(pod.Namespace, pod.Name, c.Name)); err != nil {
			return err
		}
	}
	for _, c := range pod.Spec.Containers {
		if err := m.processDb.Delete(getProcessKey(pod.Namespace, pod.Name, c.Name)); err != nil {
			return err
		}
	}
	return m.processDb.Scan(getProcessPodKey(pod), func(key []byte) error {
		origin, err := m.processDb.Get(key)
		if err != nil {
			return err
		}
		c := &ContainerProcess{}
		err = json.Unmarshal(origin, c)
		if err != nil {
			return err
		}
		proc, err := process.NewProcess(int32(c.Pid))
		if err != nil {
			return err
		}
		//TODO:优雅关闭
		return proc.KillWithContext(ctx)
	})
}

func getPodKey(ns, podName string) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", PodPrefix, ns, podName))
}

func getProcessPodKey(pod *corev1.Pod) []byte {
	return []byte(fmt.Sprintf("%s%s:%s", ProcessPrefix, pod.Namespace, pod.Name))
}

func getProcessKey(ns, podName, containerName string) []byte {
	return []byte(fmt.Sprintf("%s%s:%s:%s", ProcessPrefix, ns, podName, containerName))
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
		//解压镜像
		if err := p.im.decompressionImage(ctx, p.Container.Image, p.Workdir); err != nil {
			return err
		}
		//TODO:获取configmap和secret

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
		startHealthCheck(ctx, p, cmd.Process.Pid)
		//存储进程信息
		key := getProcessKey(p.Namespace, p.PodName, p.Container.Name)
		p.Pid = cmd.Process.Pid
		value, err := json.Marshal(p)
		if err != nil {
			return err
		}
		err = p.pm.processDb.Put(key, value)
		if err != nil {
			return err
		}
		defer func() {
			p.pm.processDb.Delete(key)
		}()
		return cmd.Wait()
	}
	if p.Sync {
		return f()
	} else {
		go f()
	}
	return nil
}

func startHealthCheck(ctx context.Context, p *ContainerProcess, pid int) {
	go func(ctx context.Context, p *ContainerProcess) {
		for {
			//在进程存在情况下,进行健康检查,进程不存在或出现错误,则跳过
			exists, err := process.PidExists(int32(pid))
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
