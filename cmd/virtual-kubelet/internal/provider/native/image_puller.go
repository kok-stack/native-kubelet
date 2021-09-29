package native

import (
	"context"
	"fmt"
	"github.com/kok-stack/native-kubelet/trace"
)

func pull(subCtx context.Context, p *ContainerProcess, bus chan ProcessEvent, podProc *PodProcess, imageName string) error {
	ctx, span := trace.StartSpan(subCtx, "pull image")
	ctx = span.WithFields(ctx, map[string]interface{}{
		"image":         imageName,
		"p.ImagePulled": p.ImagePulled,
	})
	defer span.End()
	span.Logger().Debugf("准备 pull 镜像,镜像已pull状态:%v", p.ImagePulled)
	//根据镜像pull策略判断是否要pull镜像
	if p.needPullImage() {
		bus <- BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: fmt.Sprintf("start pull image %s", p.Container.Image),
		}
		span.Logger().Debug("已发送 pull 镜像 事件")
		//TODO:下载镜像时,使用ImagePullSecret
		if err := p.im.PullImage(ctx, PullImageOpts{
			SrcImage: imageName,
			//DockerAuthConfig:            nil,
			//DockerBearerRegistryToken:   "",
			//DockerRegistryUserAgent:     "",
			//DockerInsecureSkipTLSVerify: 0,
			//Timeout: time.Hour,
			//RetryCount:                  0,
		}); err != nil {
			span.Logger().Errorf("pull 镜像出现错误,error:%v", err)
			span.SetStatus(err)
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
	span.Logger().Debug("pull 镜像完成")
	return nil
}

func unzip(subCtx context.Context, p *ContainerProcess, bus chan ProcessEvent, podProc *PodProcess) error {
	ctx, span := trace.StartSpan(subCtx, "unzip")
	ctx = span.WithFields(ctx, map[string]interface{}{
		"image":           p.Container.Image,
		"workdir":         p.Workdir,
		"p.ImageUnzipped": p.ImageUnzipped,
	})
	defer span.End()
	//解压镜像
	span.Logger().Debugf("开始解压镜像")
	if !p.ImageUnzipped {
		bus <- BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: fmt.Sprintf("start ImageUnzipped %s to workdir %s", p.Container.Image, p.Workdir),
		}
		if err := p.im.UnzipImage(subCtx, p.Container.Image, p.Workdir); err != nil {
			span.Logger().Errorf("解压镜像错误,error:%v", err)
			span.SetStatus(err)
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
	span.Logger().Debug("解压镜像完成")
	return nil
}
