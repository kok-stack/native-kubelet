package native

import (
	"context"
	"fmt"
	"github.com/flytam/filenamify"
	corev1 "k8s.io/api/core/v1"
	"path/filepath"
)

type ContainerManager struct {
	workdir string
}

func NewContainerManager(workdir string) *ContainerManager {
	return &ContainerManager{workdir: workdir}
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

	//运行
}

func getContainerWorkDir(workdir string, pod *corev1.Pod, c corev1.Container) (string, error) {
	join := filepath.Join(workdir, fmt.Sprintf("%-%-%s", pod.Namespace, pod.Name, c.Name))
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}
