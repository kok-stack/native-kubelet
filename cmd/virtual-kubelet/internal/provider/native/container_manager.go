package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flytam/filenamify"
	"github.com/prologic/bitcask"
	"github.com/shirou/gopsutil/process"
	corev1 "k8s.io/api/core/v1"
	"os"
	"os/exec"
	"path/filepath"
)

type ContainerManager struct {
	workdir   string
	processDb *bitcask.Bitcask
}

func NewContainerManager(workdir string, processDb *bitcask.Bitcask) *ContainerManager {
	return &ContainerManager{workdir: workdir, processDb: processDb}
}

type ContainerProcess struct {
	workdir   string
	podName   string
	namespace string
}

func (p *ContainerProcess) Run(ctx context.Context) error {
	//command := exec.Command()
}

/*
1.创建container
2.解压到container
3.run container
*/
func (m *ContainerManager) createContainer(ctx context.Context, im *ImageManager, c corev1.Container, pod *corev1.Pod) error {
	//获取解压的地址
	workdir, err := getContainerWorkDir(m.workdir, pod, c)
	if err != nil {
		return err
	}
	//解压
	err = im.decompressionImage(ctx, c.Image, workdir)
	if err != nil {
		return err
	}
	//配置运行process
	pro, err := m.createContainerProcess(pod, containerWorkDir(workdir))
	//运行
	return pro.Run(ctx)
}

func (m *ContainerManager) createContainerProcess(pod *corev1.Pod, workdir string) (*ContainerProcess, error) {
	process := NewProcess(pod, workdir)
	marshal, err := json.Marshal(process)
	if err != nil {
		return nil, err
	}
	err = m.processDb.Put(getProcessKey(process), marshal)
	if err != nil {
		return nil, err
	}
	return process, nil
}

func NewProcess(pod *corev1.Pod, workdir string) *ContainerProcess {
	//TODO:运行命令，环境变量等参数设置
	return &ContainerProcess{
		workdir:   workdir,
		podName:   pod.Name,
		namespace: pod.Namespace,
	}
}

func getProcessKey(process *ContainerProcess) []byte {
	return []byte(fmt.Sprintf("process:%s:%s", process.namespace, process.podName))
}

func getContainerWorkDir(workdir string, pod *corev1.Pod, c corev1.Container) (string, error) {
	join := filepath.Join(workdir, fmt.Sprintf("%-%-%s", pod.Namespace, pod.Name, c.Name))
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}
