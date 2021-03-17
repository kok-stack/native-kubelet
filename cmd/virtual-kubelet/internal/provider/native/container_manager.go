package native

import (
	"context"
	corev1 "k8s.io/api/core/v1"
)

type ContainerManager struct {
}

func (m *ContainerManager) createContainer(ctx context.Context, im *ImageManager, c corev1.Container) {

}
