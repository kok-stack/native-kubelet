# native-kubelet
native-kubelet

```shell
apt install libbtrfs-dev -y
apt install golang-github-proglottis-gpgme-dev -y
apt install -y pkg-config
apt install libdevmapper-dev -y

kind create cluster --config kind-config-1.yaml --name c1

export VKUBELET_POD_IP=192.168.212.222
export APISERVER_CERT_LOCATION=/mnt/d/code/cluster-kubelet/hack/skaffold/virtual-kubelet/vkubelet-mock-0-crt.pem
export APISERVER_KEY_LOCATION=/mnt/d/code/cluster-kubelet/hack/skaffold/virtual-kubelet/vkubelet-mock-0-key.pem
export JAEGER_ENDPOINT=127.0.0.1:14268

docker run -d --name jaeger -e COLLECTOR_ZIPKIN_HTTP_PORT=9411 -p 5775:5775/udp -p 6831:6831/udp -p 6832:6832/udp -p 5778:5778 -p 16686:16686 -p 14268:14268 -p 14250:14250 -p 9411:9411 jaegertracing/all-in-one:1.22

/mnt/d/code/native-kubelet/bin/virtual-kubelet --provider-config=/mnt/d/code/native-kubelet/example/config.json --kubeconfig=/root/.kube/config --nodename vkubelet-mock-0 --provider native-kubelet --startup-timeout 10s --klog.v "2" --klog.logtostderr --log-level debug --trace-exporter=jaeger --trace-sample-rate=always
```