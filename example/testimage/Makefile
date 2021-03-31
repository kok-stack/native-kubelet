.PHONY: build
build:
	go build main.go -o main &&	docker build -t ccr.ccs.tencentyun.com/k8s-test/test:oci-test-v1 . && docker push ccr.ccs.tencentyun.com/k8s-test/test:oci-test-v1