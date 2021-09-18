build:
	curl curl -L https://github.com/etcd-io/etcd/releases/download/v3.5.0/etcd-v3.5.0-linux-amd64.tar.gz -o etcd-v3.5.0-linux-amd64.tar.gz -o etcd.tar.gz
	tar zxvf etcd.tar.gz
	docker build -t ccr.ccs.tencentyun.com/k8s-test/test:etcd-v1 . && docker push ccr.ccs.tencentyun.com/k8s-test/test:etcd-v1