package native

import (
	"os"
	"time"
)

type ProcessEvent interface {
	getPodProcess() *PodProcess
	getContainerProcess() *ContainerProcess
	getError() error
	getMsg() string
}
type BaseProcessEvent struct {
	p   *PodProcess
	cp  *ContainerProcess
	err error
	msg string
}

type DownloadResource struct {
	BaseProcessEvent
}

func (b BaseProcessEvent) getPodProcess() *PodProcess {
	return b.p
}

func (b BaseProcessEvent) getContainerProcess() *ContainerProcess {
	return b.cp
}

func (b BaseProcessEvent) getError() error {
	return b.err
}

func (b BaseProcessEvent) getMsg() string {
	return b.msg
}

var podProcessKey = ""
var indexKey = "index"

type PodProcessStart struct {
	ProcessEvent
	t time.Time
}

type ContainerProcessError struct {
	ProcessEvent
	index int
}

type ContainerProcessRun struct {
	ProcessEvent
	pid   int
	index int
}

type ContainerProcessFinish struct {
	ProcessEvent
	pid   int
	index int
	state *os.ProcessState
}

type ContainerProcessNext struct {
	ProcessEvent
	index int
}
