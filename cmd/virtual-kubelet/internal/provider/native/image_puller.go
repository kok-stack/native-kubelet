package native

import (
	"context"
	"fmt"
	"github.com/kok-stack/native-kubelet/trace"
)

func pull(subCtx context.Context, span trace.Span, p *ContainerProcess, bus chan ProcessEvent, podProc *PodProcess, imageName string) error {
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
	return nil
}

func unzip(subCtx context.Context, span trace.Span, p *ContainerProcess, bus chan ProcessEvent, podProc *PodProcess) error {
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
	return nil
}
